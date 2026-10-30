// Package web is Twinsweep's browser app behind `twinsweep serve` / `ui`: a
// small HTTP server on this computer only (127.0.0.1) that lets the user
// pick folders and scan them, review and export results, load saved reports,
// choose and remove duplicates with live progress, and read the action log.
//
// Where it sits: cmd/twinsweep starts it. It calls internal/scan to scan,
// internal/keep for "Auto-select", and internal/clean to remove. It never
// removes anything by itself - every removal goes through clean.Run with all
// of its re-verification. Files in this package:
//
//	server.go  - setup, security middleware, routing, page + static files
//	jobs.go    - background scan and cleanup jobs, progress, cancellation
//	api.go     - folder browser, report export/load, auto-select, action log
//
// Why it needs real security even though it's "only localhost": any web page
// open in ANY browser tab can try to send requests to 127.0.0.1, and other
// user accounts on the same machine can connect to the port too. Since this
// server can list folders and delete files, it defends against:
//
//   - Other local users / programs: every request needs a random session
//     token held in an httpOnly cookie. The token is handed out through a
//     ONE-TIME link printed in the terminal; once used, the link stops
//     working, so a link glimpsed in a process list is useless afterwards.
//   - CSRF (a malicious website making your browser submit a request): every
//     state-changing request must carry the token in a custom header (which
//     other sites can't add without our permission, and we never grant it -
//     no CORS headers are sent), a matching Origin, and a JSON content type.
//     The cookie is also SameSite=Strict.
//   - DNS rebinding (a malicious domain re-pointed at 127.0.0.1 to look
//     "same-origin"): the Host header must be exactly 127.0.0.1:<port> or
//     localhost:<port>.
//   - XSS via hostile file names (e.g. a file named "<img onerror=...>"): the
//     page inserts names only as plain text, and a strict Content Security
//     Policy forbids inline scripts so injected markup couldn't run anyway.
//   - Targeting arbitrary paths for removal: cleanup requests name copies by
//     report entry ID, never by path, so only things a scan found (and
//     cleanup re-verifies) can be touched.
package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"twinsweep/internal/report"
	"twinsweep/internal/scan"
	"twinsweep/internal/version"
)

// static holds the page's HTML/JS/CSS, compiled into the binary by
// `go:embed` so the tool is a single file with nothing to install.
//
//go:embed static
var static embed.FS

// csp is the Content Security Policy: the browser may only load scripts,
// styles and data from this same server, never inline code, never in a
// frame on another site, and may not submit forms anywhere.
const csp = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// Request body limits, so a runaway or malicious request can't exhaust
// memory. maxBody (8 MiB, ~1 million selected IDs) covers ordinary API
// calls; maxReportBody allows uploading a large saved report.
const (
	maxBody       = 8 << 20
	maxReportBody = 256 << 20
)

// Config is what `serve` passes in.
type Config struct {
	Report    *report.Report     // a report to open with, or nil
	ScanOpts  *scan.Options      // a scan to start immediately, or nil
	LogPath   string             // action log override ("" = default)
	TrashFunc func(string) error // nil = real OS trash (tests override)
}

// Server holds all state for one `serve` session.
type Server struct {
	cfg  Config
	port int
	// ctx is the lifetime of the whole server; cancelling it (Ctrl+C or
	// Quit) stops any running scan or cleanup before its next item.
	ctx context.Context

	mu sync.Mutex // guards everything below

	// Current report and which of its copies were removed this session.
	rep        *report.Report
	repVersion int // bumped whenever rep or removed changes; the page reloads on change
	removed    map[int]bool

	// Scan job state (jobs.go).
	scanning   bool
	scanCancel context.CancelFunc
	progress   scan.Progress
	scanErr    string
	lastScan   *scanRequest // options of the latest scan, to prefill the form / rescan

	// Cleanup job state (jobs.go). nil until the first cleanup.
	job      *cleanJob
	jobCount int

	bootstrap string // one-time sign-in code ("" once used)
	session   string // session token (cookie + CSRF header value)

	quitOnce sync.Once
	quit     chan struct{}
	index    *template.Template
}

// randomToken returns 32 random bytes as hex, from the OS's cryptographic
// random generator (unguessable, unlike math/rand).
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// New creates a server for the given port (the port must already be bound
// by the caller - see cmd/twinsweep - so the Host check knows it). ctx is
// the server's lifetime. If cfg.ScanOpts is set, that scan starts at once.
func New(ctx context.Context, cfg Config, port int) (*Server, error) {
	sess, err := randomToken()
	if err != nil {
		return nil, err
	}
	boot, err := randomToken()
	if err != nil {
		return nil, err
	}
	idx, err := template.ParseFS(static, "static/index.html")
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg: cfg, port: port, ctx: ctx, session: sess, bootstrap: boot,
		removed: map[int]bool{}, quit: make(chan struct{}), index: idx,
	}
	if cfg.Report != nil {
		s.rep, s.repVersion = cfg.Report, 1
		s.lastScan = requestFromReport(cfg.Report)
	}
	if cfg.ScanOpts != nil {
		req := requestFromOptions(*cfg.ScanOpts)
		if err := s.startScan(*req); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Done is closed when the user clicks Quit on the page.
func (s *Server) Done() <-chan struct{} { return s.quit }

// SignInURL returns the current one-time sign-in link.
func (s *Server) SignInURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fmt.Sprintf("http://127.0.0.1:%d/?t=%s", s.port, s.bootstrap)
}

// NewSignInURL invalidates any unused link and issues a fresh one (for
// opening the page in a second browser, or after the first link was used).
func (s *Server) NewSignInURL() (string, error) {
	t, err := randomToken()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.bootstrap = t
	s.mu.Unlock()
	return s.SignInURL(), nil
}

// cookieName includes the port: browsers share cookies between ports of
// the same host, so two Twinsweep windows would otherwise clobber each
// other's sessions.
func (s *Server) cookieName() string { return "twinsweep_" + strconv.Itoa(s.port) }

// Handler returns the full HTTP handler with all security layers applied.
// Every route except "/" (which handles sign-in itself) requires a session;
// every POST additionally requires the CSRF checks in mutating().
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// Route patterns: "METHOD /path". {$} means exactly "/", nothing after.
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /static/{name}", s.withSession(s.handleStatic))
	// Read-only endpoints.
	mux.HandleFunc("GET /api/status", s.withSession(s.handleStatus))
	mux.HandleFunc("GET /api/report", s.withSession(s.handleReport))
	mux.HandleFunc("GET /api/report/export", s.withSession(s.handleExport))
	mux.HandleFunc("GET /api/browse", s.withSession(s.handleBrowse))
	mux.HandleFunc("GET /api/clean/status", s.withSession(s.handleCleanStatus))
	mux.HandleFunc("GET /api/log", s.withSession(s.handleLog))
	// State-changing endpoints.
	mux.HandleFunc("POST /api/scan", s.withSession(s.mutating(maxBody, s.handleScan)))
	mux.HandleFunc("POST /api/scan/cancel", s.withSession(s.mutating(maxBody, s.handleScanCancel)))
	mux.HandleFunc("POST /api/report/load", s.withSession(s.mutating(maxReportBody, s.handleLoad)))
	mux.HandleFunc("POST /api/select", s.withSession(s.mutating(maxBody, s.handleSelect)))
	mux.HandleFunc("POST /api/clean", s.withSession(s.mutating(maxBody, s.handleClean)))
	mux.HandleFunc("POST /api/clean/cancel", s.withSession(s.mutating(maxBody, s.handleCleanCancel)))
	mux.HandleFunc("POST /api/shutdown", s.withSession(s.mutating(maxBody, s.handleShutdown)))
	return s.secure(mux)
}

// secure is "middleware" (a wrapper run before every request): it sets the
// security headers and rejects any request whose Host isn't our own address
// (DNS-rebinding defence).
func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff") // never guess a file's type
		h.Set("X-Frame-Options", "DENY")           // older-browser clickjacking guard
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store") // file listings shouldn't linger in the browser cache
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		if !s.hostAllowed(r.Host) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostAllowed accepts only our exact loopback address and port.
func (s *Server) hostAllowed(host string) bool {
	p := ":" + strconv.Itoa(s.port)
	return host == "127.0.0.1"+p || host == "localhost"+p
}

// hasSession checks the session cookie in constant time (so response timing
// can't be used to guess the token one character at a time).
func (s *Server) hasSession(r *http.Request) bool {
	c, err := r.Cookie(s.cookieName())
	return err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.session)) == 1
}

// withSession rejects requests without a valid session with 401.
func (s *Server) withSession(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.hasSession(r) {
			writeError(w, http.StatusUnauthorized, "not signed in - open the link printed in the terminal")
			return
		}
		h(w, r)
	}
}

// mutating adds the CSRF checks required for any request that changes
// something (see the package comment), and caps the body at limit bytes.
func (s *Server) mutating(limit int64, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host {
			writeError(w, http.StatusForbidden, "cross-origin request refused")
			return
		}
		// Sec-Fetch-Site is set by modern browsers; anything but
		// same-origin means another site initiated this request.
		if sf := r.Header.Get("Sec-Fetch-Site"); sf != "" && sf != "same-origin" {
			writeError(w, http.StatusForbidden, "cross-site request refused")
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Twinsweep-Token")), []byte(s.session)) != 1 {
			writeError(w, http.StatusForbidden, "missing or wrong request token")
			return
		}
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			writeError(w, http.StatusUnsupportedMediaType, "expected JSON")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		h(w, r)
	}
}

// writeJSON sends v as a JSON response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError sends {"error": msg}. Messages are plain text meant for the
// user; no stack traces or internals are ever included.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// decode reads a JSON body into v, rejecting unknown fields so a typo'd or
// unexpected request fails loudly instead of being half-understood.
func decode(r *http.Request, v any) error {
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	return d.Decode(v)
}

// trashName is the OS's own word for its trash, for button labels.
func trashName() string {
	if runtime.GOOS == "windows" {
		return "Recycle Bin"
	}
	return "Trash"
}

// handleIndex serves the page. With ?t=<one-time code> it signs the browser
// in (sets the session cookie, burns the code) and redirects to a clean URL
// so the code doesn't stay in the address bar or history.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if t := r.URL.Query().Get("t"); t != "" {
		s.mu.Lock()
		ok := s.bootstrap != "" && subtle.ConstantTimeCompare([]byte(t), []byte(s.bootstrap)) == 1
		if ok {
			s.bootstrap = "" // one-time: burn it
		}
		s.mu.Unlock()
		if ok {
			// Accepted risk (approved 2026-10-30): Secure is left off on
			// purpose. This is plain HTTP over the loopback interface, so the
			// cookie never crosses a network, and some browsers (Safari) drop
			// Secure cookies on http://127.0.0.1, which would break sign-in.
			// nosemgrep: go.lang.security.audit.net.cookie-missing-secure.cookie-missing-secure
			http.SetCookie(w, &http.Cookie{ // #nosec G124 -- loopback-only HTTP; HttpOnly + SameSite=Strict set; see comment above
				Name: s.cookieName(), Value: s.session, Path: "/",
				HttpOnly: true,                    // page scripts can't read it
				SameSite: http.SameSiteStrictMode, // never sent on requests started by other sites
			})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	}
	if !s.hasSession(r) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintln(w, "This sign-in link has already been used or is invalid.\nPress Enter in the terminal running `twinsweep serve` to print a fresh link.")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// html/template escapes the values, so even the token can't break out
	// of its attribute.
	_ = s.index.Execute(w, map[string]string{"Token": s.session, "Version": version.Version, "TrashName": trashName()})
}

// handleStatic serves app.js / style.css by exact name (no directory
// listing, no path traversal: {name} can't contain a slash).
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	types := map[string]string{"app.js": "text/javascript; charset=utf-8", "style.css": "text/css; charset=utf-8"}
	ct, ok := types[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	b, err := fs.ReadFile(static, "static/"+name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ct)
	_, _ = w.Write(b)
}

// handleStatus is polled by the page (about once a second) for everything
// that changes over time: scan progress, cleanup progress, and whether the
// report changed. Kept small - the report itself comes from /api/report.
func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]any{
		"version": version.Version, "trashName": trashName(), "os": runtime.GOOS,
		"scanning": s.scanning, "progress": s.progress, "scanError": s.scanErr,
		"hasReport": s.rep != nil, "reportVersion": s.repVersion, "lastScan": s.lastScan,
		"cleaning": s.job != nil && s.job.Running,
	}
	if s.job != nil {
		out["clean"] = s.job.snapshot(false)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleShutdown lets the page's Quit button stop the server. Refused while
// a cleanup is running so a half-finished removal run isn't abandoned by a
// stray click; the user stops the cleanup first.
func (s *Server) handleShutdown(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	busy := s.job != nil && s.job.Running
	if !busy && s.scanCancel != nil {
		s.scanCancel()
	}
	s.mu.Unlock()
	if busy {
		writeError(w, http.StatusConflict, "a cleanup is running - stop it first")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	s.quitOnce.Do(func() { close(s.quit) })
}
