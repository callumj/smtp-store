package classify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"smtp-store/internal/config"
)

const queueSize = 1024

var normalizeCategoryKeywords = map[string][]string{
	"person": {
		"person",
		"human",
		"people",
	},
	"animal": {
		"animal",
		"fox",
		"cat",
		"dog",
		"deer",
		"bear",
		"bird",
		"raccoon",
	},
}

var normalizeCategoryOrder = []string{"person", "animal"}

// Service runs asynchronous video classification and sidecar persistence.
type Service struct {
	storageRootAbs string
	provider       Provider
	extractor      FrameExtractor
	model          string
	confidence     float64
	frameCount     int
	retryMax       int
	workerCount    int
	backfillWindow time.Duration
	storeRaw       bool
	verbose        bool
	logger         *slog.Logger
	retryBackoff   func(attempt int) time.Duration

	queue  chan string
	queued map[string]struct{}
	mu     sync.Mutex

	ffmpegMissingWarned atomic.Bool
}

// NewService creates classification service from config.
func NewService(cfg *config.Config, logger *slog.Logger) (*Service, error) {
	rootAbs, err := filepath.Abs(cfg.StorageRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve storage root: %w", err)
	}

	provider, err := newProvider(cfg)
	if err != nil {
		return nil, err
	}

	return &Service{
		storageRootAbs: rootAbs,
		provider:       provider,
		extractor:      FFmpegExtractor{},
		model:          cfg.Classification.Model,
		confidence:     cfg.Classification.ConfidenceThreshold,
		frameCount:     cfg.Classification.FrameCount,
		retryMax:       cfg.Classification.RetryMax,
		workerCount:    cfg.Classification.WorkerConcurrency,
		backfillWindow: cfg.ClassificationBackfillWindowDuration(),
		storeRaw:       cfg.ClassificationStoreRawResponseEnabled(),
		verbose:        cfg.VerboseLogs,
		logger:         logger,
		retryBackoff: func(attempt int) time.Duration {
			return time.Duration(attempt*attempt) * time.Second
		},
		queue:  make(chan string, queueSize),
		queued: make(map[string]struct{}),
	}, nil
}

func newProvider(cfg *config.Config) (Provider, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Classification.Provider)) {
	case "gemini":
		return NewGeminiProvider(cfg.Classification.APIKey, cfg.Classification.Model), nil
	default:
		return nil, fmt.Errorf("unsupported classification provider: %q", cfg.Classification.Provider)
	}
}

// Start launches worker processing and startup backfill.
func (s *Service) Start(ctx context.Context) {
	for i := 0; i < s.workerCount; i++ {
		go s.worker(ctx)
	}
	go s.backfill(ctx, time.Now())
}

// Enqueue adds a video path for async classification.
func (s *Service) Enqueue(videoPath string) bool {
	if !IsVideoPath(videoPath) || IsDetectionSidecarPath(videoPath) {
		return false
	}
	abs, err := filepath.Abs(videoPath)
	if err != nil {
		return false
	}
	if abs != s.storageRootAbs && !strings.HasPrefix(abs, s.storageRootAbs+string(os.PathSeparator)) {
		return false
	}

	s.mu.Lock()
	if _, exists := s.queued[abs]; exists {
		s.mu.Unlock()
		return false
	}
	s.queued[abs] = struct{}{}
	s.mu.Unlock()

	select {
	case s.queue <- abs:
		return true
	default:
		s.mu.Lock()
		delete(s.queued, abs)
		s.mu.Unlock()
		s.logger.Warn("classification queue full; dropping video", "video_path", abs)
		return false
	}
}

func (s *Service) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case videoPath := <-s.queue:
			s.processWithRetry(ctx, videoPath)
			s.mu.Lock()
			delete(s.queued, videoPath)
			s.mu.Unlock()
		}
	}
}

func (s *Service) processWithRetry(ctx context.Context, videoPath string) {
	base := sidecarForVideo(s.storageRootAbs, videoPath, s.provider.Name(), s.model)
	base.State = StatePending
	base.Attempts = 0
	_ = WriteSidecar(videoPath, base)

	var lastErr error
	attempts := s.retryMax + 1
	for attempt := 1; attempt <= attempts; attempt++ {
		select {
		case <-ctx.Done():
			return
		default:
		}

		sidecar := base
		sidecar.Attempts = attempt
		sidecar.State = StatePending
		sidecar.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		_ = WriteSidecar(videoPath, sidecar)

		frames, err := s.extractor.ExtractFrames(ctx, videoPath, s.frameCount)
		if err != nil {
			if errors.Is(err, ErrFFmpegUnavailable) {
				s.logFFmpegMissing(videoPath)
				sidecar.State = StateSkipped
				sidecar.LastError = err.Error()
				sidecar.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
				_ = WriteSidecar(videoPath, sidecar)
				return
			}
			lastErr = err
		} else {
			resp, err := s.provider.ClassifyVideo(ctx, ProviderRequest{VideoPath: videoPath, Frames: frames})
			if err != nil {
				lastErr = err
			} else {
				normalized := normalizeDetections(resp.Detections, s.confidence)
				sidecar.State = StateSuccess
				sidecar.LastError = ""
				sidecar.Detections = normalized
				sidecar.HasPerson = hasCategory(normalized, "person")
				sidecar.HasAnimal = hasCategory(normalized, "animal")
				if s.storeRaw {
					sidecar.RawResponse = resp.RawResponse
				}
				sidecar.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
				if err := WriteSidecar(videoPath, sidecar); err != nil {
					s.logger.Error("failed writing classification sidecar", "video_path", videoPath, "error", err)
				}
				if s.verbose {
					s.logger.Info("video classification completed", "video_path", videoPath, "detections", len(normalized), "has_person", sidecar.HasPerson, "has_animal", sidecar.HasAnimal)
				}
				return
			}
		}

		if attempt < attempts {
			backoff := time.Duration(attempt*attempt) * time.Second
			if s.retryBackoff != nil {
				backoff = s.retryBackoff(attempt)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
		}
	}

	failed := base
	failed.State = StateFailed
	failed.Attempts = attempts
	if lastErr != nil {
		failed.LastError = lastErr.Error()
	}
	failed.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := WriteSidecar(videoPath, failed); err != nil {
		s.logger.Error("failed writing failed classification sidecar", "video_path", videoPath, "error", err)
	}
	s.logger.Warn("video classification failed", "video_path", videoPath, "error", lastErr)
}

func (s *Service) logFFmpegMissing(videoPath string) {
	if s.ffmpegMissingWarned.CompareAndSwap(false, true) {
		s.logger.Warn("ffmpeg not found; video classification skipped", "video_path", videoPath)
		return
	}
	if s.verbose {
		s.logger.Warn("video classification skipped because ffmpeg is unavailable", "video_path", videoPath)
	}
}

func (s *Service) backfill(ctx context.Context, now time.Time) {
	cutoff := now.Add(-s.backfillWindow)
	_ = filepath.WalkDir(s.storageRootAbs, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if IsDetectionSidecarPath(path) || !IsVideoPath(path) {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		if info.ModTime().Before(cutoff) {
			return nil
		}

		if sc, err := LoadSidecar(path); err == nil {
			if sc.State == StateSuccess {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return context.Canceled
		default:
			s.Enqueue(path)
			return nil
		}
	})
}

func normalizeDetections(in []Detection, threshold float64) []Detection {
	out := make([]Detection, 0, len(in))
	for _, d := range in {
		confidence := d.Confidence
		if confidence > 1 && confidence <= 100 {
			confidence = confidence / 100.0
		}
		if confidence < threshold {
			continue
		}
		category := normalizeCategory(d.Category, d.Label)
		if category == "" {
			continue
		}
		out = append(out, Detection{
			Category:   category,
			Label:      strings.TrimSpace(strings.ToLower(d.Label)),
			Confidence: confidence,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].Confidence > out[j].Confidence
	})
	return out
}

func normalizeCategory(category, label string) string {
	combined := strings.ToLower(strings.TrimSpace(category + " " + label))
	if combined == "" {
		return ""
	}

	for _, normalized := range normalizeCategoryOrder {
		keywords := normalizeCategoryKeywords[normalized]
		for _, keyword := range keywords {
			if strings.Contains(combined, keyword) {
				return normalized
			}
		}
	}
	return ""
}

func hasCategory(in []Detection, category string) bool {
	for _, d := range in {
		if d.Category == category {
			return true
		}
	}
	return false
}
