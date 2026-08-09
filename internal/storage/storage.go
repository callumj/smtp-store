package storage

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode"

	"html"
	"os"

	"github.com/emersion/go-message/mail"
)

var htmlTagRe = regexp.MustCompile(`<[^>]+>`)

// Store persists incoming messages and attachments to disk.
type Store struct {
	root string
}

// Result describes where message content was written.
type Result struct {
	Recipient       string
	Directory       string
	BodyPath        string
	AttachmentPaths []string
}

// New creates a disk-backed storage writer.
func New(root string) *Store {
	return &Store{root: root}
}

// CheckWritable verifies the storage root is writable from the current process namespace.
func CheckWritable(root string) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("create storage root: %w", err)
	}
	file, err := os.CreateTemp(root, ".smtp-store-health-*")
	if err != nil {
		return fmt.Errorf("create storage health file: %w", err)
	}
	name := file.Name()
	if _, err := file.WriteString("ok\n"); err != nil {
		_ = file.Close()
		_ = os.Remove(name)
		return fmt.Errorf("write storage health file: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("close storage health file: %w", err)
	}
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("remove storage health file: %w", err)
	}
	return nil
}

// IsUnavailableError reports whether an error indicates the backing storage is not usable.
func IsUnavailableError(err error) bool {
	if err == nil {
		return false
	}
	for _, target := range []error{
		syscall.EROFS,
		syscall.ESTALE,
		syscall.ENOTCONN,
		syscall.ENODEV,
		syscall.ENXIO,
		syscall.EHOSTDOWN,
		syscall.EHOSTUNREACH,
	} {
		if errors.Is(err, target) {
			return true
		}
	}

	message := strings.ToLower(err.Error())
	for _, needle := range []string{
		"read-only file system",
		"stale file handle",
		"transport endpoint is not connected",
		"host is down",
		"no route to host",
	} {
		if strings.Contains(message, needle) {
			return true
		}
	}
	return false
}

// ProcessAndStore parses a message and writes body+attachments to disk.
func (s *Store) ProcessAndStore(rawMessage []byte, receivedAt time.Time) (Result, error) {
	parsed, err := parseMessage(rawMessage)
	if err != nil {
		return Result{}, err
	}

	normalizedRecipient := NormalizeRecipientFolder(parsed.Recipient)
	dir := filepath.Join(
		s.root,
		normalizedRecipient,
		fmt.Sprintf("%04d", receivedAt.Year()),
		fmt.Sprintf("%02d", int(receivedAt.Month())),
		fmt.Sprintf("%02d", receivedAt.Day()),
	)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Result{}, fmt.Errorf("create output dir: %w", err)
	}

	prefix, bodyPath, err := writeBodyWithCollisionHandling(dir, receivedAt.Format("150405"), parsed.Body)
	if err != nil {
		return Result{}, err
	}

	attachmentPaths := make([]string, 0, len(parsed.Attachments))
	for i, a := range parsed.Attachments {
		name := fmt.Sprintf("%s_%d%s", prefix, i+1, a.Extension)
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, a.Data, 0o644); err != nil {
			return Result{}, fmt.Errorf("write attachment %q: %w", name, err)
		}
		attachmentPaths = append(attachmentPaths, path)
	}

	return Result{
		Recipient:       parsed.Recipient,
		Directory:       dir,
		BodyPath:        bodyPath,
		AttachmentPaths: attachmentPaths,
	}, nil
}

func writeBodyWithCollisionHandling(dir, timestamp, body string) (prefix, bodyPath string, err error) {
	for attempt := 1; ; attempt++ {
		candidate := timestamp
		if attempt > 1 {
			candidate = fmt.Sprintf("%s-%d", timestamp, attempt)
		}

		path := filepath.Join(dir, candidate+".txt")
		file, openErr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(openErr, os.ErrExist) {
			continue
		}
		if openErr != nil {
			return "", "", fmt.Errorf("open body output: %w", openErr)
		}

		if _, writeErr := file.WriteString(body); writeErr != nil {
			_ = file.Close()
			return "", "", fmt.Errorf("write body output: %w", writeErr)
		}
		if closeErr := file.Close(); closeErr != nil {
			return "", "", fmt.Errorf("close body output: %w", closeErr)
		}
		return candidate, path, nil
	}
}

// NormalizeRecipientFolder sanitizes email addresses for directory usage.
func NormalizeRecipientFolder(address string) string {
	in := strings.TrimSpace(strings.ToLower(address))
	if in == "" {
		return "unknown"
	}

	var b strings.Builder
	b.Grow(len(in))
	for _, r := range in {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			b.WriteRune(r)
		case strings.ContainsRune("@._+-", r):
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}

	out := strings.Trim(b.String(), "._")
	if out == "" {
		return "unknown"
	}
	return out
}

type parsedMessage struct {
	Recipient   string
	Body        string
	Attachments []parsedAttachment
}

type parsedAttachment struct {
	Extension string
	Data      []byte
}

func parseMessage(raw []byte) (parsedMessage, error) {
	reader, err := mail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		return parsedMessage{}, fmt.Errorf("parse MIME message: %w", err)
	}

	to, err := reader.Header.AddressList("To")
	if err != nil {
		return parsedMessage{}, fmt.Errorf("parse To header: %w", err)
	}
	if len(to) == 0 {
		return parsedMessage{}, errors.New("message does not contain To recipient")
	}
	firstRecipient := strings.TrimSpace(to[0].Address)
	if firstRecipient == "" {
		return parsedMessage{}, errors.New("first To recipient is empty")
	}

	var plainBody string
	var htmlBody string
	attachments := make([]parsedAttachment, 0, 4)

	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return parsedMessage{}, fmt.Errorf("read MIME part: %w", err)
		}

		switch h := part.Header.(type) {
		case *mail.InlineHeader:
			mediaType, _, _ := h.ContentType()
			bodyBytes, readErr := io.ReadAll(part.Body)
			if readErr != nil {
				return parsedMessage{}, fmt.Errorf("read inline part: %w", readErr)
			}
			bodyText := string(bodyBytes)
			switch {
			case strings.EqualFold(mediaType, "text/plain"):
				if plainBody == "" {
					plainBody = bodyText
				}
			case strings.EqualFold(mediaType, "text/html"):
				if htmlBody == "" {
					htmlBody = htmlToText(bodyText)
				}
			default:
				if plainBody == "" {
					plainBody = bodyText
				}
			}
		case *mail.AttachmentHeader:
			filename, _ := h.Filename()
			mediaType, _, _ := h.ContentType()
			payload, readErr := io.ReadAll(part.Body)
			if readErr != nil {
				return parsedMessage{}, fmt.Errorf("read attachment part: %w", readErr)
			}
			attachments = append(attachments, parsedAttachment{
				Extension: extensionFrom(filename, mediaType),
				Data:      payload,
			})
		}
	}

	body := plainBody
	if body == "" {
		body = htmlBody
	}

	return parsedMessage{
		Recipient:   firstRecipient,
		Body:        body,
		Attachments: attachments,
	}, nil
}

func extensionFrom(filename, mediaType string) string {
	ext := strings.ToLower(strings.TrimSpace(filepath.Ext(filename)))
	if ext != "" {
		return ext
	}
	if strings.TrimSpace(mediaType) != "" {
		if exts, err := mime.ExtensionsByType(mediaType); err == nil && len(exts) > 0 {
			return strings.ToLower(exts[0])
		}
	}
	return ".bin"
}

func htmlToText(raw string) string {
	stripped := htmlTagRe.ReplaceAllString(raw, " ")
	decoded := html.UnescapeString(stripped)
	return strings.Join(strings.Fields(decoded), " ")
}
