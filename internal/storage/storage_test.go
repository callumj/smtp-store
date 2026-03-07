package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalizeRecipientFolder(t *testing.T) {
	t.Parallel()
	got := NormalizeRecipientFolder(" CAM/ERA+alerts@Example.COM\n")
	want := "cam_era+alerts@example.com"
	if got != want {
		t.Fatalf("NormalizeRecipientFolder() = %q, want %q", got, want)
	}
}

func TestParseMessagePlainBodyAndAttachment(t *testing.T) {
	t.Parallel()
	raw := strings.Join([]string{
		"From: sender@example.com",
		"To: first@example.com, second@example.com",
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
		"Content-Disposition: attachment; filename=clip.MP4",
		"Content-Transfer-Encoding: base64",
		"",
		"AAEC",
		"--abc123--",
		"",
	}, "\r\n")

	msg, err := parseMessage([]byte(raw))
	if err != nil {
		t.Fatalf("parseMessage() error = %v", err)
	}

	if msg.Recipient != "first@example.com" {
		t.Fatalf("recipient = %q", msg.Recipient)
	}
	if strings.TrimSpace(msg.Body) != "motion detected" {
		t.Fatalf("body = %q", msg.Body)
	}
	if len(msg.Attachments) != 1 {
		t.Fatalf("attachments = %d", len(msg.Attachments))
	}
	if msg.Attachments[0].Extension != ".mp4" {
		t.Fatalf("attachment extension = %q", msg.Attachments[0].Extension)
	}
	if len(msg.Attachments[0].Data) == 0 {
		t.Fatal("expected decoded attachment data")
	}
}

func TestParseMessageHTMLFallback(t *testing.T) {
	t.Parallel()
	raw := strings.Join([]string{
		"From: sender@example.com",
		"To: first@example.com",
		"Subject: motion",
		"MIME-Version: 1.0",
		"Content-Type: text/html; charset=utf-8",
		"",
		"<html><body><h1>Alert</h1><p>Motion &amp; audio</p></body></html>",
		"",
	}, "\r\n")

	msg, err := parseMessage([]byte(raw))
	if err != nil {
		t.Fatalf("parseMessage() error = %v", err)
	}

	if msg.Body != "Alert Motion & audio" {
		t.Fatalf("html fallback body = %q", msg.Body)
	}
}

func TestParseMessageAttachmentWithoutFilenameUsesMimeExtension(t *testing.T) {
	t.Parallel()
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
		"Content-Type: image/jpeg",
		"Content-Disposition: attachment",
		"Content-Transfer-Encoding: base64",
		"",
		"/9j/2Q==",
		"--abc123--",
		"",
	}, "\r\n")

	msg, err := parseMessage([]byte(raw))
	if err != nil {
		t.Fatalf("parseMessage() error = %v", err)
	}

	if len(msg.Attachments) != 1 {
		t.Fatalf("attachments = %d", len(msg.Attachments))
	}
	if msg.Attachments[0].Extension != ".jpe" && msg.Attachments[0].Extension != ".jpg" && msg.Attachments[0].Extension != ".jpeg" && msg.Attachments[0].Extension != ".jfif" {
		t.Fatalf("unexpected mime-derived extension: %q", msg.Attachments[0].Extension)
	}
}

func TestProcessAndStoreCollisionHandling(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := New(dir)
	now := time.Date(2026, 3, 7, 11, 23, 21, 0, time.Local)

	raw := strings.Join([]string{
		"From: sender@example.com",
		"To: alerts@example.com",
		"Subject: motion",
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"motion detected",
		"",
	}, "\r\n")

	first, err := store.ProcessAndStore([]byte(raw), now)
	if err != nil {
		t.Fatalf("first ProcessAndStore() error = %v", err)
	}
	second, err := store.ProcessAndStore([]byte(raw), now)
	if err != nil {
		t.Fatalf("second ProcessAndStore() error = %v", err)
	}

	if !strings.HasSuffix(first.BodyPath, filepath.Join("2026", "03", "07", "112321.txt")) {
		t.Fatalf("unexpected first body path: %q", first.BodyPath)
	}
	if !strings.HasSuffix(second.BodyPath, filepath.Join("2026", "03", "07", "112321-2.txt")) {
		t.Fatalf("unexpected second body path: %q", second.BodyPath)
	}

	if _, err := os.Stat(first.BodyPath); err != nil {
		t.Fatalf("missing first body file: %v", err)
	}
	if _, err := os.Stat(second.BodyPath); err != nil {
		t.Fatalf("missing second body file: %v", err)
	}
}
