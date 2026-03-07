package smtpserver

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	netsmtp "net/smtp"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"smtp-store/internal/config"
	"smtp-store/internal/storage"
)

func TestSMTPAuthRequired(t *testing.T) {
	t.Parallel()
	addr, _, cleanup := startTestServer(t, false)
	defer cleanup()

	client, err := netsmtp.Dial(addr)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer client.Close()

	err = client.Mail("sender@example.com")
	if err == nil {
		t.Fatal("expected MAIL to fail without AUTH")
	}
	if !strings.Contains(err.Error(), "530") {
		t.Fatalf("expected 530 auth required error, got %v", err)
	}
}

func TestSMTPAuthenticatedSendWritesFiles(t *testing.T) {
	t.Parallel()
	addr, storageRoot, cleanup := startTestServer(t, false)
	defer cleanup()

	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort() error = %v", err)
	}

	client, err := netsmtp.Dial(addr)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer client.Close()

	if err := client.Auth(netsmtp.PlainAuth("", "camera", "secret", host)); err != nil {
		t.Fatalf("Auth() error = %v", err)
	}
	if err := client.Mail("sender@example.com"); err != nil {
		t.Fatalf("Mail() error = %v", err)
	}
	if err := client.Rcpt("envelope@example.com"); err != nil {
		t.Fatalf("Rcpt() error = %v", err)
	}

	w, err := client.Data()
	if err != nil {
		t.Fatalf("Data() error = %v", err)
	}
	message := strings.Join([]string{
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
	if _, err := io.WriteString(w, message); err != nil {
		t.Fatalf("write Data() error = %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Data writer close error = %v", err)
	}
	if err := client.Quit(); err != nil {
		t.Fatalf("Quit() error = %v", err)
	}

	today := time.Now()
	dir := filepath.Join(storageRoot, "first@example.com", fmt.Sprintf("%04d", today.Year()), fmt.Sprintf("%02d", int(today.Month())), fmt.Sprintf("%02d", today.Day()))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q) error = %v", dir, err)
	}

	var foundBody bool
	var foundAttachment bool
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".txt") {
			foundBody = true
		}
		if strings.Contains(name, "_1.") && strings.HasSuffix(strings.ToLower(name), ".mp4") {
			foundAttachment = true
		}
	}

	if !foundBody {
		t.Fatal("expected body .txt file")
	}
	if !foundAttachment {
		t.Fatal("expected attachment file")
	}
}

func TestSMTPStartTLSSend(t *testing.T) {
	t.Parallel()
	addr, storageRoot, cleanup := startTestServer(t, true)
	defer cleanup()

	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort() error = %v", err)
	}

	client, err := netsmtp.Dial(addr)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer client.Close()

	ok, _ := client.Extension("STARTTLS")
	if !ok {
		t.Fatal("expected STARTTLS extension")
	}
	if err := client.StartTLS(&tls.Config{ServerName: host, InsecureSkipVerify: true}); err != nil {
		t.Fatalf("StartTLS() error = %v", err)
	}
	if err := client.Auth(netsmtp.PlainAuth("", "camera", "secret", host)); err != nil {
		t.Fatalf("Auth() error = %v", err)
	}
	if err := client.Mail("sender@example.com"); err != nil {
		t.Fatalf("Mail() error = %v", err)
	}
	if err := client.Rcpt("envelope@example.com"); err != nil {
		t.Fatalf("Rcpt() error = %v", err)
	}
	w, err := client.Data()
	if err != nil {
		t.Fatalf("Data() error = %v", err)
	}
	if _, err := io.WriteString(w, strings.Join([]string{
		"From: sender@example.com",
		"To: tls@example.com",
		"Subject: motion",
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"tls motion detected",
		"",
	}, "\r\n")); err != nil {
		t.Fatalf("write Data() error = %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Data writer close error = %v", err)
	}

	today := time.Now()
	expectedDir := filepath.Join(storageRoot, "tls@example.com", fmt.Sprintf("%04d", today.Year()), fmt.Sprintf("%02d", int(today.Month())), fmt.Sprintf("%02d", today.Day()))
	if _, err := os.Stat(expectedDir); err != nil {
		t.Fatalf("expected output directory to exist: %v", err)
	}
}

func startTestServer(t *testing.T, withTLS bool) (addr, storageRoot string, cleanup func()) {
	t.Helper()

	storageRoot = t.TempDir()
	cfg := &config.Config{
		ListenAddr:  "127.0.0.1:0",
		StorageRoot: storageRoot,
		Hostname:    "localhost",
		Users: []config.UserCreds{{
			Username: "camera",
			Password: "secret",
		}},
	}

	if withTLS {
		certPath, keyPath := writeSelfSignedCert(t)
		cfg.TLS.Enabled = true
		cfg.TLS.CertFile = certPath
		cfg.TLS.KeyFile = keyPath
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := storage.New(storageRoot)
	srv, err := New(cfg, store, logger)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	addr = ln.Addr().String()

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ln)
	}()

	cleanup = func() {
		_ = srv.Close()
		_ = ln.Close()
		select {
		case err := <-errCh:
			if err != nil && !strings.Contains(strings.ToLower(err.Error()), "closed") {
				t.Fatalf("server returned error: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting for server shutdown")
		}
	}

	return addr, storageRoot, cleanup
}

func writeSelfSignedCert(t *testing.T) (certPath, keyPath string) {
	t.Helper()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}

	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}

	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("CreateCertificate() error = %v", err)
	}

	certBuf := &bytes.Buffer{}
	if err := pem.Encode(certBuf, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("encode cert: %v", err)
	}

	keyBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey() error = %v", err)
	}
	keyBuf := &bytes.Buffer{}
	if err := pem.Encode(keyBuf, &pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes}); err != nil {
		t.Fatalf("encode key: %v", err)
	}

	dir := t.TempDir()
	certPath = filepath.Join(dir, "server.crt")
	keyPath = filepath.Join(dir, "server.key")
	if err := os.WriteFile(certPath, certBuf.Bytes(), 0o644); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyBuf.Bytes(), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	return certPath, keyPath
}
