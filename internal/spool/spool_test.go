package spool

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"log/slog"

	"smtp-store/internal/storage"
)

func TestQueueEnqueueAndFlushStoresMessage(t *testing.T) {
	t.Parallel()

	spoolRoot := t.TempDir()
	storageRoot := t.TempDir()
	store := storage.New(storageRoot)
	var postStoreCalls atomic.Int64
	q, err := New(spoolRoot, 10<<20, time.Hour, store, slog.New(slog.NewTextHandler(io.Discard, nil)), func(result storage.Result) {
		postStoreCalls.Add(1)
		if result.BodyPath == "" {
			t.Error("post store result missing body path")
		}
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	receivedAt := time.Date(2026, 8, 9, 18, 30, 0, 0, time.UTC)
	id, err := q.Enqueue([]byte(rawMessage("camera@example.com")), receivedAt)
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	if id == "" {
		t.Fatal("expected spool id")
	}

	q.flush(context.Background())

	bodyPath := filepath.Join(storageRoot, "camera@example.com", "2026", "08", "09", "183000.txt")
	body, err := os.ReadFile(bodyPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", bodyPath, err)
	}
	if !strings.Contains(string(body), "motion detected") {
		t.Fatalf("unexpected body: %q", body)
	}
	if postStoreCalls.Load() != 1 {
		t.Fatalf("post store calls = %d, want 1", postStoreCalls.Load())
	}
	if entries, err := os.ReadDir(filepath.Join(spoolRoot, pendingDir)); err != nil || len(entries) != 0 {
		t.Fatalf("pending entries = %d, err = %v; want empty", len(entries), err)
	}
}

func TestQueueEnqueueRejectsWhenFull(t *testing.T) {
	t.Parallel()

	q, err := New(t.TempDir(), 32, time.Hour, storage.New(t.TempDir()), slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := q.Enqueue([]byte(rawMessage("camera@example.com")), time.Now()); err == nil {
		t.Fatal("expected spool full error")
	}
}

func rawMessage(to string) string {
	return strings.Join([]string{
		"From: sender@example.com",
		"To: " + to,
		"Subject: motion",
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"motion detected",
		"",
	}, "\r\n")
}
