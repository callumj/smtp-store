package spool

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"smtp-store/internal/storage"
)

const (
	pendingDir    = "pending"
	processingDir = "processing"
	failedDir     = "failed"
	tmpDir        = "tmp"
)

// PostStoreHandler receives messages after they are successfully written to final storage.
type PostStoreHandler func(storage.Result)

// Queue is a local durable spool for raw SMTP messages.
type Queue struct {
	path          string
	maxBytes      int64
	flushInterval time.Duration
	store         *storage.Store
	logger        *slog.Logger
	postStore     PostStoreHandler
	mu            sync.Mutex
}

type itemMeta struct {
	ReceivedAt string `json:"received_at"`
}

// New creates a local durable spool queue.
func New(path string, maxBytes int64, flushInterval time.Duration, store *storage.Store, logger *slog.Logger, postStore PostStoreHandler) (*Queue, error) {
	q := &Queue{
		path:          path,
		maxBytes:      maxBytes,
		flushInterval: flushInterval,
		store:         store,
		logger:        logger,
		postStore:     postStore,
	}
	for _, dir := range []string{pendingDir, processingDir, failedDir, tmpDir} {
		if err := os.MkdirAll(filepath.Join(path, dir), 0o750); err != nil {
			return nil, fmt.Errorf("create spool %s dir: %w", dir, err)
		}
	}
	if err := storage.CheckWritable(filepath.Join(path, tmpDir)); err != nil {
		return nil, fmt.Errorf("spool path is not writable: %w", err)
	}
	return q, nil
}

// Enqueue persists a raw SMTP message to local disk.
func (q *Queue) Enqueue(raw []byte, receivedAt time.Time) (string, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.maxBytes > 0 {
		size, err := q.sizeLocked()
		if err != nil {
			return "", err
		}
		if size+int64(len(raw)) > q.maxBytes {
			return "", fmt.Errorf("spool full: %d bytes queued, %d byte message, %d byte limit", size, len(raw), q.maxBytes)
		}
	}

	id := q.newID(receivedAt)
	tmpPath := filepath.Join(q.path, tmpDir, id+".eml.tmp")
	pendingPath := filepath.Join(q.path, pendingDir, id+".eml")

	file, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return "", fmt.Errorf("create spool item: %w", err)
	}
	meta := itemMeta{ReceivedAt: receivedAt.Format(time.RFC3339Nano)}
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		_ = file.Close()
		_ = os.Remove(tmpPath)
		return "", err
	}
	if _, err := file.Write(append(metaBytes, '\n')); err != nil {
		_ = file.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("write spool metadata: %w", err)
	}
	if _, err := file.Write(raw); err != nil {
		_ = file.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("write spool message: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("sync spool message: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("close spool message: %w", err)
	}
	if err := os.Rename(tmpPath, pendingPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("publish spool item: %w", err)
	}
	return id, nil
}

// Start launches background processing for queued messages.
func (q *Queue) Start(ctx context.Context) {
	go q.run(ctx)
}

func (q *Queue) run(ctx context.Context) {
	q.recoverProcessing()
	q.flush(ctx)

	ticker := time.NewTicker(q.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			q.flush(ctx)
		}
	}
}

func (q *Queue) flush(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		path, ok := q.nextPending()
		if !ok {
			return
		}
		if ok := q.processOne(path); !ok {
			return
		}
	}
}

func (q *Queue) processOne(pendingPath string) bool {
	base := filepath.Base(pendingPath)
	processingPath := filepath.Join(q.path, processingDir, base)
	if err := os.Rename(pendingPath, processingPath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			q.logger.Warn("failed claiming spool item", "path", pendingPath, "error", err)
		}
		return true
	}

	meta, raw, err := readItem(processingPath)
	if err != nil {
		q.moveToFailed(processingPath)
		q.logger.Error("failed reading spool item", "path", processingPath, "error", err)
		return true
	}
	receivedAt, err := time.Parse(time.RFC3339Nano, meta.ReceivedAt)
	if err != nil {
		receivedAt = time.Now()
	}

	result, err := q.store.ProcessAndStore(raw, receivedAt)
	if err != nil {
		if storage.IsUnavailableError(err) {
			_ = os.Rename(processingPath, pendingPath)
			q.logger.Warn("storage unavailable; retaining spooled message", "spool_item", base, "error", err)
			return false
		}
		q.moveToFailed(processingPath)
		q.logger.Error("failed processing spooled message", "spool_item", base, "error", err)
		return true
	}

	if err := os.Remove(processingPath); err != nil {
		q.logger.Warn("failed removing processed spool item", "path", processingPath, "error", err)
	}
	q.logger.Info("spooled message stored", "spool_item", base, "body_path", result.BodyPath, "attachments", len(result.AttachmentPaths))
	if q.postStore != nil {
		q.postStore(result)
	}
	return true
}

func (q *Queue) recoverProcessing() {
	entries, err := os.ReadDir(filepath.Join(q.path, processingDir))
	if err != nil {
		q.logger.Warn("failed reading processing spool dir", "error", err)
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		from := filepath.Join(q.path, processingDir, entry.Name())
		to := filepath.Join(q.path, pendingDir, entry.Name())
		if err := os.Rename(from, to); err != nil {
			q.logger.Warn("failed recovering processing spool item", "path", from, "error", err)
		}
	}
}

func (q *Queue) nextPending() (string, bool) {
	entries, err := os.ReadDir(filepath.Join(q.path, pendingDir))
	if err != nil {
		q.logger.Warn("failed reading pending spool dir", "error", err)
		return "", false
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".eml") {
			names = append(names, entry.Name())
		}
	}
	if len(names) == 0 {
		return "", false
	}
	sort.Strings(names)
	return filepath.Join(q.path, pendingDir, names[0]), true
}

func (q *Queue) moveToFailed(path string) {
	to := filepath.Join(q.path, failedDir, filepath.Base(path))
	if err := os.Rename(path, to); err != nil {
		q.logger.Warn("failed moving spool item to failed dir", "path", path, "error", err)
	}
}

func (q *Queue) sizeLocked() (int64, error) {
	var total int64
	for _, dir := range []string{pendingDir, processingDir} {
		root := filepath.Join(q.path, dir)
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += info.Size()
			return nil
		})
		if err != nil {
			return 0, fmt.Errorf("measure spool size: %w", err)
		}
	}
	return total, nil
}

func (q *Queue) newID(receivedAt time.Time) string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%s-%d", receivedAt.UTC().Format("20060102T150405.000000000Z"), time.Now().UnixNano())
	}
	return receivedAt.UTC().Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(buf[:])
}

func readItem(path string) (itemMeta, []byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return itemMeta{}, nil, err
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	metaLine, err := reader.ReadBytes('\n')
	if err != nil {
		return itemMeta{}, nil, fmt.Errorf("read metadata: %w", err)
	}
	var meta itemMeta
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(metaLine))), &meta); err != nil {
		return itemMeta{}, nil, fmt.Errorf("parse metadata: %w", err)
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		return itemMeta{}, nil, fmt.Errorf("read message: %w", err)
	}
	return meta, raw, nil
}
