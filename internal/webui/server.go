package webui

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"smtp-store/internal/classify"
	"smtp-store/internal/config"
)

const (
	cookieName  = "smtp_store_session"
	recentLimit = 80
)

//go:embed templates/*.html static/*
var webAssets embed.FS

type contextKey string

const userContextKey contextKey = "ui_user"

type app struct {
	rootAbs       string
	uiUsers       map[string]string
	sessionSecret []byte
	sessionTTL    time.Duration
	verbose       bool
	logger        *slog.Logger
	templates     *template.Template
}

type recipientEntry struct {
	Name      string
	BrowseURL string
}

type recentItem struct {
	Name         string
	Recipient    string
	Relative     string
	Size         string
	Modified     string
	HasPerson    bool
	HasAnimal    bool
	DetectState  string
	DetectLabels []string
	ViewURL      string
	DownloadURL  string
}

type browseEntry struct {
	Name         string
	Relative     string
	IsDir        bool
	Size         string
	Modified     string
	HasPerson    bool
	HasAnimal    bool
	DetectState  string
	DetectLabels []string
	BrowseURL    string
	ViewURL      string
	DownloadURL  string
}

type crumb struct {
	Label string
	URL   string
}

type pageCommon struct {
	Title       string
	CurrentUser string
}

type loginPageData struct {
	Title string
	Error string
}

type dashboardPageData struct {
	pageCommon
	StorageRoot string
	Recipients  []recipientEntry
	Recent      []recentItem
}

type browsePageData struct {
	pageCommon
	CurrentPath string
	Breadcrumbs []crumb
	Entries     []browseEntry
}

type textViewPageData struct {
	pageCommon
	Name        string
	Relative    string
	Content     string
	Modified    string
	DownloadURL string
}

type binaryViewPageData struct {
	pageCommon
	Name        string
	Relative    string
	ContentType string
	Size        string
	Modified    string
	DownloadURL string
}

// New creates an HTTP server for browsing captured SMTP content.
func New(cfg *config.Config, logger *slog.Logger) (*http.Server, error) {
	h, err := NewHandler(cfg, logger)
	if err != nil {
		return nil, err
	}

	return &http.Server{
		Addr:         cfg.Web.ListenAddr,
		Handler:      h,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  60 * time.Second,
	}, nil
}

// NewHandler creates the UI HTTP handler.
func NewHandler(cfg *config.Config, logger *slog.Logger) (http.Handler, error) {
	templates, err := template.New("ui").ParseFS(webAssets, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse UI templates: %w", err)
	}

	rootAbs, err := filepath.Abs(cfg.StorageRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve storage root: %w", err)
	}

	a := &app{
		rootAbs:       rootAbs,
		uiUsers:       cfg.UIUserMap(),
		sessionSecret: []byte(cfg.Web.SessionSecret),
		sessionTTL:    cfg.WebSessionTTLDuration(),
		verbose:       cfg.VerboseLogs,
		logger:        logger,
		templates:     templates,
	}

	mux := http.NewServeMux()
	staticFS, err := fs.Sub(webAssets, "static")
	if err != nil {
		return nil, fmt.Errorf("load UI static assets: %w", err)
	}

	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))
	mux.HandleFunc("/login", a.handleLogin)
	mux.Handle("/logout", a.requireAuth(http.HandlerFunc(a.handleLogout)))
	mux.Handle("/browse/", a.requireAuth(http.HandlerFunc(a.handleBrowse)))
	mux.Handle("/browse", a.requireAuth(http.HandlerFunc(a.handleBrowseRootRedirect)))
	mux.Handle("/view/", a.requireAuth(http.HandlerFunc(a.handleView)))
	mux.Handle("/download/", a.requireAuth(http.HandlerFunc(a.handleDownload)))
	mux.Handle("/", a.requireAuth(http.HandlerFunc(a.handleDashboard)))

	return mux, nil
}

func (a *app) handleLogin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.render(w, "login.html", loginPageData{Title: "SMTP Store Login"})
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}

		username := strings.ToLower(strings.TrimSpace(r.FormValue("username")))
		password := r.FormValue("password")
		stored, ok := a.uiUsers[username]
		if !ok || stored != password {
			a.logger.Warn("web login failed", "username", username, "remote_addr", remoteAddr(r))
			w.WriteHeader(http.StatusUnauthorized)
			a.render(w, "login.html", loginPageData{Title: "SMTP Store Login", Error: "Invalid username or password."})
			return
		}

		expiresAt := time.Now().Add(a.sessionTTL)
		token := a.createSessionToken(username, expiresAt)
		http.SetCookie(w, &http.Cookie{
			Name:     cookieName,
			Value:    token,
			Path:     "/",
			HttpOnly: true,
			Secure:   isSecureRequest(r),
			SameSite: http.SameSiteLaxMode,
			Expires:  expiresAt,
			MaxAge:   int(a.sessionTTL.Seconds()),
		})

		a.logger.Info("web login succeeded", "username", username, "remote_addr", remoteAddr(r))
		http.Redirect(w, r, "/", http.StatusSeeOther)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *app) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   isSecureRequest(r),
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
	})

	a.logger.Info("web logout", "username", currentUser(r), "remote_addr", remoteAddr(r))
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (a *app) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	recipients, err := a.listRecipients()
	if err != nil {
		http.Error(w, "failed to list recipients", http.StatusInternalServerError)
		return
	}

	recent, err := a.listRecentFiles(recentLimit)
	if err != nil {
		http.Error(w, "failed to load recent files", http.StatusInternalServerError)
		return
	}

	a.render(w, "dashboard.html", dashboardPageData{
		pageCommon:  pageCommon{Title: "SMTP Store", CurrentUser: currentUser(r)},
		StorageRoot: a.rootAbs,
		Recipients:  recipients,
		Recent:      recent,
	})
}

func (a *app) handleBrowseRootRedirect(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/browse" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/browse/", http.StatusSeeOther)
}

func (a *app) handleBrowse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	relSlash, absPath, err := a.resolvePathFromRequest(r, "/browse/")
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	info, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "failed to stat path", http.StatusInternalServerError)
		return
	}

	if !info.IsDir() {
		http.Redirect(w, r, "/view/"+escapeRelPath(relSlash), http.StatusSeeOther)
		return
	}

	entries, err := os.ReadDir(absPath)
	if err != nil {
		http.Error(w, "failed to read directory", http.StatusInternalServerError)
		return
	}

	browseEntries := make([]browseEntry, 0, len(entries))
	for _, entry := range entries {
		if classify.IsDetectionSidecarPath(entry.Name()) {
			continue
		}
		entryInfo, err := entry.Info()
		if err != nil {
			continue
		}

		childRel := joinRel(relSlash, entry.Name())
		item := browseEntry{
			Name:     entry.Name(),
			Relative: childRel,
			IsDir:    entry.IsDir(),
			Modified: entryInfo.ModTime().Local().Format("2006-01-02 15:04:05"),
		}
		if entry.IsDir() {
			item.BrowseURL = "/browse/" + escapeRelPath(childRel)
			item.Size = "-"
		} else {
			item.Size = humanSize(entryInfo.Size())
			item.DetectState, item.HasPerson, item.HasAnimal, item.DetectLabels = a.detectionSummaryForFile(filepath.Join(absPath, entry.Name()))
			item.ViewURL = "/view/" + escapeRelPath(childRel)
			item.DownloadURL = "/download/" + escapeRelPath(childRel)
		}
		browseEntries = append(browseEntries, item)
	}

	sort.Slice(browseEntries, func(i, j int) bool {
		if browseEntries[i].IsDir != browseEntries[j].IsDir {
			return browseEntries[i].IsDir
		}
		return strings.ToLower(browseEntries[i].Name) < strings.ToLower(browseEntries[j].Name)
	})

	a.render(w, "browse.html", browsePageData{
		pageCommon:  pageCommon{Title: "Browse", CurrentUser: currentUser(r)},
		CurrentPath: relSlash,
		Breadcrumbs: breadcrumbs(relSlash),
		Entries:     browseEntries,
	})
}

func (a *app) handleView(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	relSlash, absPath, err := a.resolvePathFromRequest(r, "/view/")
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	if relSlash == "" {
		http.Redirect(w, r, "/browse/", http.StatusSeeOther)
		return
	}

	info, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "failed to stat file", http.StatusInternalServerError)
		return
	}
	if info.IsDir() {
		http.Redirect(w, r, "/browse/"+escapeRelPath(relSlash), http.StatusSeeOther)
		return
	}

	name := filepath.Base(absPath)
	if strings.EqualFold(filepath.Ext(name), ".txt") {
		body, err := os.ReadFile(absPath)
		if err != nil {
			http.Error(w, "failed to read text body", http.StatusInternalServerError)
			return
		}
		a.render(w, "text_view.html", textViewPageData{
			pageCommon:  pageCommon{Title: name, CurrentUser: currentUser(r)},
			Name:        name,
			Relative:    relSlash,
			Content:     string(body),
			Modified:    info.ModTime().Local().Format("2006-01-02 15:04:05"),
			DownloadURL: "/download/" + escapeRelPath(relSlash),
		})
		return
	}

	contentType, err := detectContentType(absPath)
	if err != nil {
		http.Error(w, "failed to detect file type", http.StatusInternalServerError)
		return
	}

	if isInlinePreviewType(contentType) {
		disposition := mime.FormatMediaType("inline", map[string]string{"filename": name})
		w.Header().Set("Content-Disposition", disposition)
		w.Header().Set("Content-Type", contentType)
		http.ServeFile(w, r, absPath)
		return
	}

	a.render(w, "binary_view.html", binaryViewPageData{
		pageCommon:  pageCommon{Title: name, CurrentUser: currentUser(r)},
		Name:        name,
		Relative:    relSlash,
		ContentType: contentType,
		Size:        humanSize(info.Size()),
		Modified:    info.ModTime().Local().Format("2006-01-02 15:04:05"),
		DownloadURL: "/download/" + escapeRelPath(relSlash),
	})
}

func (a *app) handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	_, absPath, err := a.resolvePathFromRequest(r, "/download/")
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	info, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "failed to stat file", http.StatusInternalServerError)
		return
	}
	if info.IsDir() {
		http.Error(w, "directories cannot be downloaded", http.StatusBadRequest)
		return
	}

	contentType, err := detectContentType(absPath)
	if err != nil {
		http.Error(w, "failed to detect file type", http.StatusInternalServerError)
		return
	}

	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": info.Name()})
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("Content-Type", contentType)
	http.ServeFile(w, r, absPath)
}

func (a *app) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := a.authenticatedUser(r)
		if !ok {
			if a.verbose {
				a.logger.Info("web unauthorized request", "path", r.URL.Path, "remote_addr", remoteAddr(r))
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		if a.verbose {
			a.logger.Info("web request", "method", r.Method, "path", r.URL.Path, "username", user, "remote_addr", remoteAddr(r))
		}

		ctx := context.WithValue(r.Context(), userContextKey, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *app) authenticatedUser(r *http.Request) (string, bool) {
	cookie, err := r.Cookie(cookieName)
	if err != nil {
		return "", false
	}

	username, err := a.verifySessionToken(cookie.Value)
	if err != nil {
		return "", false
	}
	if _, ok := a.uiUsers[username]; !ok {
		return "", false
	}
	return username, true
}

func currentUser(r *http.Request) string {
	value := r.Context().Value(userContextKey)
	if value == nil {
		return ""
	}
	user, _ := value.(string)
	return user
}

func (a *app) createSessionToken(username string, expiresAt time.Time) string {
	payload := fmt.Sprintf("%s|%d", username, expiresAt.Unix())
	sig := a.sign(payload)
	tokenPayload := fmt.Sprintf("%s|%s", payload, hex.EncodeToString(sig))
	return base64.RawURLEncoding.EncodeToString([]byte(tokenPayload))
}

func (a *app) verifySessionToken(token string) (string, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", err
	}

	parts := strings.Split(string(decoded), "|")
	if len(parts) != 3 {
		return "", errors.New("invalid token shape")
	}
	username := strings.TrimSpace(parts[0])
	expiresUnix, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", err
	}
	if time.Now().After(time.Unix(expiresUnix, 0)) {
		return "", errors.New("session expired")
	}

	expectedSig := a.sign(parts[0] + "|" + parts[1])
	providedSig, err := hex.DecodeString(parts[2])
	if err != nil {
		return "", err
	}
	if !hmac.Equal(expectedSig, providedSig) {
		return "", errors.New("invalid session signature")
	}
	return strings.ToLower(username), nil
}

func (a *app) sign(payload string) []byte {
	h := hmac.New(sha256.New, a.sessionSecret)
	_, _ = h.Write([]byte(payload))
	return h.Sum(nil)
}

func (a *app) render(w http.ResponseWriter, templateName string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.templates.ExecuteTemplate(w, templateName, data); err != nil {
		a.logger.Error("failed to render UI template", "template", templateName, "error", err)
		http.Error(w, "template rendering failed", http.StatusInternalServerError)
	}
}

func (a *app) resolvePathFromRequest(r *http.Request, prefix string) (relSlash, absPath string, err error) {
	rel := strings.TrimPrefix(r.URL.Path, prefix)
	rel = strings.TrimPrefix(rel, "/")
	unescaped, unescapeErr := url.PathUnescape(rel)
	if unescapeErr == nil {
		rel = unescaped
	}
	rel = strings.ReplaceAll(rel, "\\", "/")
	for _, segment := range strings.Split(rel, "/") {
		if segment == ".." {
			return "", "", errors.New("path traversal is not allowed")
		}
	}

	cleanRel := strings.TrimPrefix(path.Clean("/"+rel), "/")
	if cleanRel == "." {
		cleanRel = ""
	}

	joined := filepath.Join(a.rootAbs, filepath.FromSlash(cleanRel))
	resolved, err := filepath.Abs(joined)
	if err != nil {
		return "", "", err
	}
	if resolved != a.rootAbs && !strings.HasPrefix(resolved, a.rootAbs+string(os.PathSeparator)) {
		return "", "", errors.New("path escapes storage root")
	}

	return cleanRel, resolved, nil
}

func (a *app) listRecipients() ([]recipientEntry, error) {
	entries, err := os.ReadDir(a.rootAbs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	recipients := make([]recipientEntry, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		recipients = append(recipients, recipientEntry{
			Name:      entry.Name(),
			BrowseURL: "/browse/" + escapeRelPath(entry.Name()),
		})
	}

	sort.Slice(recipients, func(i, j int) bool {
		return strings.ToLower(recipients[i].Name) < strings.ToLower(recipients[j].Name)
	})

	return recipients, nil
}

func (a *app) listRecentFiles(limit int) ([]recentItem, error) {
	if limit <= 0 {
		return nil, nil
	}
	if _, err := os.Stat(a.rootAbs); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	type candidate struct {
		rel  string
		path string
		info os.FileInfo
	}

	candidates := make([]candidate, 0, limit)
	err := filepath.WalkDir(a.rootAbs, func(current string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if classify.IsDetectionSidecarPath(current) {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}

		rel, err := filepath.Rel(a.rootAbs, current)
		if err != nil {
			return nil
		}

		candidates = append(candidates, candidate{
			rel:  filepath.ToSlash(rel),
			path: current,
			info: info,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].info.ModTime().After(candidates[j].info.ModTime())
	})
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}

	items := make([]recentItem, 0, len(candidates))
	for _, c := range candidates {
		recipient := ""
		parts := strings.SplitN(c.rel, "/", 2)
		if len(parts) > 0 {
			recipient = parts[0]
		}
		detectState, hasPerson, hasAnimal, detectLabels := a.detectionSummaryForFile(c.path)
		items = append(items, recentItem{
			Name:         filepath.Base(c.path),
			Recipient:    recipient,
			Relative:     c.rel,
			Size:         humanSize(c.info.Size()),
			Modified:     c.info.ModTime().Local().Format("2006-01-02 15:04:05"),
			DetectState:  detectState,
			HasPerson:    hasPerson,
			HasAnimal:    hasAnimal,
			DetectLabels: detectLabels,
			ViewURL:      "/view/" + escapeRelPath(c.rel),
			DownloadURL:  "/download/" + escapeRelPath(c.rel),
		})
	}

	return items, nil
}

func breadcrumbs(rel string) []crumb {
	crumbs := []crumb{{Label: "Root", URL: "/browse/"}}
	if rel == "" {
		return crumbs
	}

	parts := strings.Split(rel, "/")
	acc := ""
	for _, part := range parts {
		acc = joinRel(acc, part)
		crumbs = append(crumbs, crumb{Label: part, URL: "/browse/" + escapeRelPath(acc)})
	}
	return crumbs
}

func joinRel(base, name string) string {
	if base == "" {
		return name
	}
	return base + "/" + name
}

func escapeRelPath(rel string) string {
	if rel == "" {
		return ""
	}
	parts := strings.Split(rel, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

func detectContentType(path string) (string, error) {
	extType := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if extType != "" {
		return extType, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	buf := make([]byte, 512)
	n, err := f.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return http.DetectContentType(buf[:n]), nil
}

func isInlinePreviewType(contentType string) bool {
	contentType = strings.ToLower(contentType)
	switch {
	case strings.HasPrefix(contentType, "image/"):
		return true
	case strings.HasPrefix(contentType, "video/"):
		return true
	case strings.HasPrefix(contentType, "audio/"):
		return true
	case strings.HasPrefix(contentType, "text/"):
		return true
	case strings.HasPrefix(contentType, "application/pdf"):
		return true
	default:
		return false
	}
}

func humanSize(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(size)/float64(div), "KMGTPE"[exp])
}

func remoteAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (a *app) detectionSummaryForFile(filePath string) (state string, hasPerson, hasAnimal bool, labels []string) {
	if !classify.IsVideoPath(filePath) {
		return "", false, false, nil
	}
	sidecar, err := classify.LoadSidecar(filePath)
	if err != nil {
		return classify.StatePending, false, false, nil
	}
	return classify.DetectionStatus(sidecar), sidecar.HasPerson, sidecar.HasAnimal, detectionLabels(sidecar)
}

func detectionLabels(sidecar *classify.Sidecar) []string {
	if sidecar == nil || len(sidecar.Detections) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(sidecar.Detections))
	out := make([]string, 0, len(sidecar.Detections))
	for _, d := range sidecar.Detections {
		label := strings.TrimSpace(strings.ToLower(d.Label))
		if label == "" {
			continue
		}
		if isGenericDetectionLabel(label, d.Category) {
			continue
		}
		if _, ok := seen[label]; ok {
			continue
		}
		seen[label] = struct{}{}
		out = append(out, label)
	}
	return out
}

func isGenericDetectionLabel(label, category string) bool {
	category = strings.TrimSpace(strings.ToLower(category))
	if label == category {
		return true
	}
	switch label {
	case "person", "people", "human", "animal":
		return true
	default:
		return false
	}
}

func isSecureRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
