package webui

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"smtp-store/internal/config"
)

func TestAuthRedirectAndLoginFlow(t *testing.T) {
	t.Parallel()
	ts, _ := startTestUIServer(t, "1h")

	resp := doRequestNoRedirect(t, ts.Client(), http.MethodGet, ts.URL+"/", nil, nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusSeeOther)
	}
	if got := resp.Header.Get("Location"); got != "/login" {
		t.Fatalf("location = %q, want /login", got)
	}

	badLogin := doRequestNoRedirect(t, ts.Client(), http.MethodPost, ts.URL+"/login", strings.NewReader(url.Values{
		"username": {"admin"},
		"password": {"wrong"},
	}.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if badLogin.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad login status = %d, want %d", badLogin.StatusCode, http.StatusUnauthorized)
	}

	cookie := loginAndGetSessionCookie(t, ts, "admin", "ui-pass")
	okResp := doRequestNoRedirect(t, ts.Client(), http.MethodGet, ts.URL+"/", nil, map[string]string{"Cookie": cookie.String()})
	if okResp.StatusCode != http.StatusOK {
		t.Fatalf("dashboard status = %d, want %d", okResp.StatusCode, http.StatusOK)
	}
	body := readBody(t, okResp)
	if !strings.Contains(body, "Recent Files") {
		t.Fatalf("dashboard body missing recent section: %s", body)
	}
}

func TestSessionExpiry(t *testing.T) {
	t.Parallel()
	ts, _ := startTestUIServer(t, "10ms")

	cookie := loginAndGetSessionCookie(t, ts, "admin", "ui-pass")
	time.Sleep(30 * time.Millisecond)

	resp := doRequestNoRedirect(t, ts.Client(), http.MethodGet, ts.URL+"/", nil, map[string]string{"Cookie": cookie.String()})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want redirect", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/login" {
		t.Fatalf("location = %q, want /login", got)
	}
}

func TestBrowseAndViewEndpoints(t *testing.T) {
	t.Parallel()
	ts, root := startTestUIServer(t, "1h")
	cookie := loginAndGetSessionCookie(t, ts, "admin", "ui-pass")

	browseURL := ts.URL + "/browse/camera@local/2026/03/07"
	browseResp := doRequestNoRedirect(t, ts.Client(), http.MethodGet, browseURL, nil, map[string]string{"Cookie": cookie.String()})
	if browseResp.StatusCode != http.StatusOK {
		t.Fatalf("browse status = %d, want %d", browseResp.StatusCode, http.StatusOK)
	}
	browseBody := readBody(t, browseResp)
	if !strings.Contains(browseBody, "112321.txt") {
		t.Fatalf("browse body missing text file entry")
	}
	if !strings.Contains(browseBody, "112321_1.mp4") {
		t.Fatalf("browse body missing attachment entry")
	}

	textViewURL := ts.URL + "/view/camera@local/2026/03/07/112321.txt"
	textResp := doRequestNoRedirect(t, ts.Client(), http.MethodGet, textViewURL, nil, map[string]string{"Cookie": cookie.String()})
	if textResp.StatusCode != http.StatusOK {
		t.Fatalf("text view status = %d, want %d", textResp.StatusCode, http.StatusOK)
	}
	if body := readBody(t, textResp); !strings.Contains(body, "motion detected") {
		t.Fatalf("text view missing content")
	}

	mediaViewURL := ts.URL + "/view/camera@local/2026/03/07/112321_1.mp4"
	mediaResp := doRequestNoRedirect(t, ts.Client(), http.MethodGet, mediaViewURL, nil, map[string]string{"Cookie": cookie.String()})
	if mediaResp.StatusCode != http.StatusOK {
		t.Fatalf("media view status = %d, want %d", mediaResp.StatusCode, http.StatusOK)
	}
	if !strings.Contains(strings.ToLower(mediaResp.Header.Get("Content-Disposition")), "inline") {
		t.Fatalf("media view should set inline disposition")
	}

	downloadURL := ts.URL + "/download/camera@local/2026/03/07/112321_1.mp4"
	downloadResp := doRequestNoRedirect(t, ts.Client(), http.MethodGet, downloadURL, nil, map[string]string{"Cookie": cookie.String()})
	if downloadResp.StatusCode != http.StatusOK {
		t.Fatalf("download status = %d, want %d", downloadResp.StatusCode, http.StatusOK)
	}
	if !strings.Contains(strings.ToLower(downloadResp.Header.Get("Content-Disposition")), "attachment") {
		t.Fatalf("download should set attachment disposition")
	}

	// Ensure dashboard recent view reflects existing files.
	dashResp := doRequestNoRedirect(t, ts.Client(), http.MethodGet, ts.URL+"/", nil, map[string]string{"Cookie": cookie.String()})
	if dashResp.StatusCode != http.StatusOK {
		t.Fatalf("dashboard status = %d, want %d", dashResp.StatusCode, http.StatusOK)
	}
	dashBody := readBody(t, dashResp)
	if !strings.Contains(dashBody, "camera@local") {
		t.Fatalf("dashboard missing recipient")
	}
	if !strings.Contains(dashBody, "112321_1.mp4") {
		t.Fatalf("dashboard missing recent file")
	}

	_ = root
}

func TestPathTraversalBlocked(t *testing.T) {
	t.Parallel()
	ts, _ := startTestUIServer(t, "1h")
	cookie := loginAndGetSessionCookie(t, ts, "admin", "ui-pass")

	paths := []string{
		"/download/%2e%2e/%2e%2e/etc/passwd",
		"/download/%2e%2e%2f%2e%2e%2fetc%2fpasswd",
		"/download/%2e%2e\\\\%2e%2e\\\\etc\\\\passwd",
	}
	for _, p := range paths {
		resp := doRequestNoRedirect(t, ts.Client(), http.MethodGet, ts.URL+p, nil, map[string]string{"Cookie": cookie.String()})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("path %q status = %d, want %d", p, resp.StatusCode, http.StatusBadRequest)
		}
	}
}

func startTestUIServer(t *testing.T, sessionTTL string) (*httptest.Server, string) {
	t.Helper()

	root := t.TempDir()
	dir := filepath.Join(root, "camera@local", "2026", "03", "07")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "112321.txt"), []byte("motion detected"), 0o644); err != nil {
		t.Fatalf("write text file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "112321_1.mp4"), []byte{0x00, 0x01, 0x02, 0x03}, 0o644); err != nil {
		t.Fatalf("write attachment file: %v", err)
	}

	cfg := &config.Config{
		StorageRoot: root,
		VerboseLogs: false,
		Web: config.WebConfig{
			ListenAddr:    "127.0.0.1:0",
			SessionTTL:    sessionTTL,
			SessionSecret: "test-session-secret",
		},
		Users: []config.UserCreds{{
			Username: "camera",
			Password: "smtp-pass",
		}},
		UIUsers: []config.UserCreds{{
			Username: "admin",
			Password: "ui-pass",
		}},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h, err := NewHandler(cfg, logger)
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts, root
}

func loginAndGetSessionCookie(t *testing.T, ts *httptest.Server, username, password string) *http.Cookie {
	t.Helper()

	resp := doRequestNoRedirect(t, ts.Client(), http.MethodPost, ts.URL+"/login", strings.NewReader(url.Values{
		"username": {username},
		"password": {password},
	}.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status = %d, want %d", resp.StatusCode, http.StatusSeeOther)
	}

	for _, cookie := range resp.Cookies() {
		if cookie.Name == cookieName {
			return cookie
		}
	}
	t.Fatal("expected session cookie")
	return nil
}

func doRequestNoRedirect(t *testing.T, client *http.Client, method, target string, body io.Reader, headers map[string]string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(method, target, body)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	nrClient := *client
	nrClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}

	resp, err := nrClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	return string(payload)
}
