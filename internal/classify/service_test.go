package classify

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"smtp-store/internal/storage"
)

type mockExtractor struct {
	err    error
	frames [][]byte
}

func (m mockExtractor) ExtractFrames(_ context.Context, _ string, _ int) ([][]byte, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.frames, nil
}

type mockProvider struct {
	calls  atomic.Int32
	fn     func() (ProviderResponse, error)
	lastIn ProviderRequest
}

func (m *mockProvider) Name() string { return "mock" }

func (m *mockProvider) ClassifyVideo(_ context.Context, req ProviderRequest) (ProviderResponse, error) {
	m.lastIn = req
	m.calls.Add(1)
	return m.fn()
}

type mockNotifier struct {
	calls   atomic.Int32
	lastVid string
	last    Sidecar
	err     error
}

func (m *mockNotifier) PublishClassification(_ context.Context, videoPath string, sidecar Sidecar) error {
	m.calls.Add(1)
	m.lastVid = videoPath
	m.last = sidecar
	return m.err
}

func TestSidecarReadWriteAndStatus(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	video := filepath.Join(root, "a.mp4")
	if err := os.WriteFile(video, []byte{1, 2, 3}, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	s := sidecarForVideo(root, video, "gemini", "model")
	s.State = StateSuccess
	if err := WriteSidecar(video, s); err != nil {
		t.Fatalf("WriteSidecar() error = %v", err)
	}

	loaded, err := LoadSidecar(video)
	if err != nil {
		t.Fatalf("LoadSidecar() error = %v", err)
	}
	if loaded.State != StateSuccess {
		t.Fatalf("state = %q", loaded.State)
	}
	if DetectionStatus(loaded) != "none" {
		t.Fatalf("status = %q", DetectionStatus(loaded))
	}
}

func TestProcessWithRetryStoresSuccessAndFiltersDetections(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	video := filepath.Join(root, "clip.mp4")
	if err := os.WriteFile(video, []byte{1, 2, 3}, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	provider := &mockProvider{fn: func() (ProviderResponse, error) {
		return ProviderResponse{
			Detections: []Detection{
				{Category: "person", Label: "person", Confidence: 0.95},
				{Category: "animal", Label: "fox", Confidence: 0.70},
				{Category: "vehicle", Label: "car", Confidence: 0.99},
				{Category: "animal", Label: "cat", Confidence: 0.30},
			},
			RawResponse: `{"detections":[...]}`,
		}, nil
	}}
	notifier := &mockNotifier{}

	svc := &Service{
		storageRootAbs: root,
		provider:       provider,
		extractor:      mockExtractor{frames: [][]byte{{1, 2, 3}}},
		model:          "mock-model",
		confidence:     0.60,
		frameCount:     6,
		retryMax:       0,
		workerCount:    1,
		backfillWindow: 7 * 24 * time.Hour,
		storeRaw:       true,
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		retryBackoff:   func(_ int) time.Duration { return 0 },
		notifier:       notifier,
		queue:          make(chan job, queueSize),
		queued:         make(map[string]struct{}),
	}

	svc.processWithRetry(context.Background(), video, true)

	sidecar, err := LoadSidecar(video)
	if err != nil {
		t.Fatalf("LoadSidecar() error = %v", err)
	}
	if sidecar.State != StateSuccess {
		t.Fatalf("state = %q, want %q", sidecar.State, StateSuccess)
	}
	if !sidecar.HasPerson || !sidecar.HasAnimal || !sidecar.HasVehicle {
		t.Fatalf("expected person+animal+vehicle true, got person=%v animal=%v vehicle=%v", sidecar.HasPerson, sidecar.HasAnimal, sidecar.HasVehicle)
	}
	if len(sidecar.Detections) != 3 {
		t.Fatalf("detections = %d, want 3", len(sidecar.Detections))
	}
	if sidecar.RawResponse == "" {
		t.Fatal("expected raw response to be stored")
	}
	if sidecar.ThumbnailPath != "clip.mp4.thumb.jpg" {
		t.Fatalf("thumbnail path = %q", sidecar.ThumbnailPath)
	}
	if _, err := os.Stat(filepath.Join(root, "clip.mp4.thumb.jpg")); err != nil {
		t.Fatalf("expected thumbnail file: %v", err)
	}
	if notifier.calls.Load() != 1 {
		t.Fatalf("notifier calls = %d, want 1", notifier.calls.Load())
	}
	if sidecar.MQTTPublishedAt == "" {
		t.Fatal("expected mqtt published timestamp")
	}
}

func TestProcessWithRetryRetriesThenSucceeds(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	video := filepath.Join(root, "clip.mp4")
	if err := os.WriteFile(video, []byte{1, 2, 3}, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	count := 0
	provider := &mockProvider{fn: func() (ProviderResponse, error) {
		count++
		if count < 3 {
			return ProviderResponse{}, errors.New("temporary failure")
		}
		return ProviderResponse{Detections: []Detection{{Category: "person", Label: "person", Confidence: 0.9}}}, nil
	}}

	svc := &Service{
		storageRootAbs: root,
		provider:       provider,
		extractor:      mockExtractor{frames: [][]byte{{1}}},
		model:          "mock-model",
		confidence:     0.60,
		frameCount:     6,
		retryMax:       3,
		workerCount:    1,
		backfillWindow: 7 * 24 * time.Hour,
		storeRaw:       true,
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		retryBackoff:   func(_ int) time.Duration { return 0 },
		queue:          make(chan job, queueSize),
		queued:         make(map[string]struct{}),
	}

	svc.processWithRetry(context.Background(), video, true)

	sidecar, err := LoadSidecar(video)
	if err != nil {
		t.Fatalf("LoadSidecar() error = %v", err)
	}
	if sidecar.State != StateSuccess {
		t.Fatalf("state = %q", sidecar.State)
	}
	if sidecar.Attempts != 3 {
		t.Fatalf("attempts = %d, want 3", sidecar.Attempts)
	}
}

func TestProcessWithRetrySkipsWhenFFmpegMissing(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	video := filepath.Join(root, "clip.mp4")
	if err := os.WriteFile(video, []byte{1, 2, 3}, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	provider := &mockProvider{fn: func() (ProviderResponse, error) {
		return ProviderResponse{Detections: []Detection{{Category: "person", Label: "person", Confidence: 0.99}}}, nil
	}}

	svc := &Service{
		storageRootAbs: root,
		provider:       provider,
		extractor:      mockExtractor{err: ErrFFmpegUnavailable},
		model:          "mock-model",
		confidence:     0.60,
		frameCount:     6,
		retryMax:       3,
		workerCount:    1,
		backfillWindow: 7 * 24 * time.Hour,
		storeRaw:       true,
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		retryBackoff:   func(_ int) time.Duration { return 0 },
		queue:          make(chan job, queueSize),
		queued:         make(map[string]struct{}),
	}

	svc.processWithRetry(context.Background(), video, true)

	sidecar, err := LoadSidecar(video)
	if err != nil {
		t.Fatalf("LoadSidecar() error = %v", err)
	}
	if sidecar.State != StateSkipped {
		t.Fatalf("state = %q, want %q", sidecar.State, StateSkipped)
	}
	if provider.calls.Load() != 0 {
		t.Fatalf("provider should not be called when ffmpeg unavailable")
	}
}

func TestBackfillWindowSkipsOldAndSuccessful(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	newPending := filepath.Join(root, "recent_pending.mp4")
	newSuccess := filepath.Join(root, "recent_success.mp4")
	oldPending := filepath.Join(root, "old_pending.mp4")
	for _, path := range []string{newPending, newSuccess, oldPending} {
		if err := os.WriteFile(path, []byte{1, 2, 3}, 0o644); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", path, err)
		}
	}
	now := time.Now()
	_ = os.Chtimes(newPending, now, now.Add(-1*time.Hour))
	_ = os.Chtimes(newSuccess, now, now.Add(-2*time.Hour))
	_ = os.Chtimes(oldPending, now, now.Add(-9*24*time.Hour))

	successSidecar := sidecarForVideo(root, newSuccess, "mock", "model")
	successSidecar.State = StateSuccess
	if err := WriteSidecar(newSuccess, successSidecar); err != nil {
		t.Fatalf("WriteSidecar() error = %v", err)
	}

	svc := &Service{
		storageRootAbs: root,
		provider:       &mockProvider{fn: func() (ProviderResponse, error) { return ProviderResponse{}, nil }},
		extractor:      mockExtractor{frames: [][]byte{{1}}},
		model:          "mock-model",
		confidence:     0.60,
		frameCount:     6,
		retryMax:       3,
		workerCount:    1,
		backfillWindow: 7 * 24 * time.Hour,
		storeRaw:       true,
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		retryBackoff:   func(_ int) time.Duration { return 0 },
		queue:          make(chan job, queueSize),
		queued:         make(map[string]struct{}),
	}

	svc.backfill(context.Background(), now)

	if len(svc.queue) != 1 {
		t.Fatalf("queue len = %d, want 1", len(svc.queue))
	}
	enqueued := <-svc.queue
	if filepath.Base(enqueued.videoPath) != filepath.Base(newPending) {
		t.Fatalf("unexpected enqueued file: %q", enqueued.videoPath)
	}
	if enqueued.notify {
		t.Fatal("backfill queue job should not publish notifications")
	}
}

func TestIntegrationIngestAndClassifyCreatesSidecar(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := storage.New(root)

	provider := &mockProvider{fn: func() (ProviderResponse, error) {
		return ProviderResponse{Detections: []Detection{{Category: "person", Label: "person", Confidence: 0.9}}}, nil
	}}

	svc := &Service{
		storageRootAbs: root,
		provider:       provider,
		extractor:      mockExtractor{frames: [][]byte{{1}}},
		model:          "mock-model",
		confidence:     0.60,
		frameCount:     6,
		retryMax:       1,
		workerCount:    1,
		backfillWindow: 7 * 24 * time.Hour,
		storeRaw:       true,
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		retryBackoff:   func(_ int) time.Duration { return 0 },
		queue:          make(chan job, queueSize),
		queued:         make(map[string]struct{}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx)

	raw := strings.Join([]string{
		"From: sender@example.com",
		"To: first@example.com",
		"Subject: motion",
		"MIME-Version: 1.0",
		"Content-Type: multipart/mixed; boundary=abc123",
		"",
		"--abc123",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"motion detected",
		"--abc123",
		"Content-Type: video/mp4",
		"Content-Disposition: attachment; filename=clip.mp4",
		"Content-Transfer-Encoding: base64",
		"",
		"AAEC",
		"--abc123--",
		"",
	}, "\r\n")

	result, err := store.ProcessAndStore([]byte(raw), time.Now())
	if err != nil {
		t.Fatalf("ProcessAndStore() error = %v", err)
	}
	if len(result.AttachmentPaths) == 0 {
		t.Fatal("expected attachment path")
	}
	videoPath := result.AttachmentPaths[0]
	svc.Enqueue(videoPath)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		sidecar, err := LoadSidecar(videoPath)
		if err == nil && sidecar.State == StateSuccess {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("timed out waiting for classification sidecar")
}
