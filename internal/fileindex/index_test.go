package fileindex

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"smtp-store/internal/classify"
)

func TestIndexBackfillRecentAndSidecarMetadata(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	dir := filepath.Join(root, "garden@local", "2026", "06", "29")
	mustMkdirAll(t, dir)
	text := filepath.Join(dir, "120000.txt")
	video := filepath.Join(dir, "120000_1.mp4")
	mustWriteFile(t, text, []byte("motion"))
	mustWriteFile(t, video, []byte{0x00, 0x01})

	sidecar := classify.Sidecar{
		SchemaVersion: classify.SchemaVersion,
		RelativePath:  "garden@local/2026/06/29/120000_1.mp4",
		State:         classify.StateSuccess,
		Provider:      "test",
		Model:         "test-model",
		HasPerson:     true,
		HasVehicle:    true,
		Detections: []classify.Detection{
			{Category: "person", Label: "person", Confidence: 0.99},
			{Category: "animal", Label: "fox", Confidence: 0.82},
			{Category: "vehicle", Label: "car", Confidence: 0.91},
		},
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := classify.WriteSidecar(video, sidecar); err != nil {
		t.Fatalf("WriteSidecar() error = %v", err)
	}

	idx, err := Open(filepath.Join(t.TempDir(), "index.sqlite"), root, slog.Default())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })

	if err := idx.Backfill(context.Background()); err != nil {
		t.Fatalf("Backfill() error = %v", err)
	}

	recipients, err := idx.Recipients()
	if err != nil {
		t.Fatalf("Recipients() error = %v", err)
	}
	if len(recipients) != 1 || recipients[0] != "garden@local" {
		t.Fatalf("recipients = %#v, want garden@local", recipients)
	}

	recent, err := idx.Recent(10)
	if err != nil {
		t.Fatalf("Recent() error = %v", err)
	}
	if len(recent) != 2 {
		t.Fatalf("recent count = %d, want 2", len(recent))
	}
	var videoItem File
	for _, item := range recent {
		if item.RelativePath == "garden@local/2026/06/29/120000_1.mp4" {
			videoItem = item
		}
		if filepath.Base(item.RelativePath) == "120000_1.mp4.detections.json" {
			t.Fatalf("sidecar should not be indexed: %#v", item)
		}
	}
	if videoItem.RelativePath == "" {
		t.Fatal("video item not found")
	}
	if videoItem.DetectState != "detected" || !videoItem.HasPerson || videoItem.HasAnimal || !videoItem.HasVehicle {
		t.Fatalf("unexpected detection fields: %#v", videoItem)
	}
	if len(videoItem.DetectLabels) != 1 || videoItem.DetectLabels[0] != "fox" {
		t.Fatalf("labels = %#v, want [fox]", videoItem.DetectLabels)
	}
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", path, err)
	}
}

func mustWriteFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}
