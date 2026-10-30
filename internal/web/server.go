// Package web is the local review page behind `twinsweep serve`: a small
// HTTP server on this computer only (127.0.0.1) that shows scan results in a
// browser and lets the user pick which copies to remove.
//
// Where it sits: cmd/twinsweep starts it; it calls internal/scan to scan,
// internal/keep for "Auto-select", and internal/clean to remove. It never
// removes anything by itself - every removal goes through clean.Run with all
// of its re-verification.
//
// Why it needs real security even though it's "only localhost": any web page
// open in ANY browser tab can try to send requests to 127.0.0.1, and other
// user accounts on the same machine can connect to the port too. Since this
// server can delete files, it defends against:
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
//   - Targeting arbitrary paths: requests name copies by report entry ID,
//     never by path, so only things the scan found can be touched.
package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"twinsweep/internal/clean"
	"twinsweep/internal/keep"
	"twinsweep/internal/pathutil"
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

// maxBody caps request bodies (8 MiB is ~1 million selected IDs) so a
// runaway or malicious request can't exhaust memory.
const maxBody = 8 << 20

// Config is what `serve` passes in.
type Config struct {
	Report    *report.Report     // a loaded report, or nil to scan with ScanOpts
	ScanOpts  scan.Options       // used when Report is nil
	LogPath   string             // action log override ("" = default)
	TrashFunc func(string) error // nil = real OS trash (tests override)
}

// Server holds all state for one `serve` session.
type Server struct {
	cfg  Config
	port int

	mu        sync.Mutex // guards everything below
	state     string     // "scanning", "ready", "error"
	scanErr   string
	progress  scan.Progress
	rep       *report.Report
	removed   map[int]bool // entry IDs removed during this session
	busy      bool         // a cleanup is running
	bootstrap string       // one-time sign-in code ("" once used)
	session   string       // session token (cookie + CSRF header value)

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
// by the caller - see cmd/twinsweep - so the Host check knows it).
func New(cfg Config, port int) (*Server, error) {
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
	s := &Server{cfg: cfg, port: port, session: sess, bootstrap: boot, removed: map[int]bool{}, quit: make(chan struct{}), index: idx}
	if cfg.Report != nil {
		s.rep, s.state = cfg.Report, "ready"
	} else {
		s.state = "scanning"
	}
	return s, nil
}

// StartScan runs the scan in the background (when no report was given).
// ctx cancels it, e.g. on Ctrl+C.
func (s *Server) StartScan(ctx context.Context) {
	if s.cfg.Report != nil {
		return
	}
	opts := s.cfg.ScanOpts
	opts.Progress = func(p scan.Progress) {
		s.mu.Lock()
		s.progress = p
		s.mu.Unlock()
	}
	go func() {
		rep, err := scan.Run(ctx, opts)
		s.mu.Lock()
		defer s.mu.Unlock()
		if err != nil {
			s.state, s.scanErr = "error", err.Error()
			return
		}
		s.rep, s.state = rep, "ready"
	}()
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
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// Route patterns: "METHOD /path". {$} means exactly "/", nothing after.
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /static/{name}", s.withSession(s.handleStatic))
	mux.HandleFunc("GET /api/status", s.withSession(s.handleStatus))
	mux.HandleFunc("GET /api/report", s.withSession(s.handleReport))
	mux.HandleFunc("POST /api/select", s.withSession(s.mutating(s.handleSelect)))
	mux.HandleFunc("POST /api/clean", s.withSession(s.mutating(s.handleClean)))
	mux.HandleFunc("POST /api/shutdown", s.withSession(s.mutating(s.handleShutdown)))
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
// something (see the package comment), and caps the body size.
func (s *Server) mutating(h http.HandlerFunc) http.HandlerFunc {
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
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
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

// handleStatus reports scan progress / readiness for the page to poll.
func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"state": s.state, "error": s.scanErr, "progress": s.progress,
		"busy": s.busy, "version": version.Version, "trashName": trashName(),
	})
}

// liveReport returns a copy of the report without copies already removed
// this session, dropping groups left with fewer than two copies. Caller
// must hold s.mu.
func (s *Server) liveReport() *report.Report {
	cp := *s.rep
	filter := func(gs []report.Group) []report.Group {
		out := []report.Group{}
		for _, g := range gs {
			var es []report.Entry
			for _, e := range g.Entries {
				if !s.removed[e.ID] {
					es = append(es, e)
				}
			}
			if len(es) >= 2 {
				g.Entries = es
				out = append(out, g)
			}
		}
		return out
	}
	cp.FolderGroups = filter(s.rep.FolderGroups)
	cp.FileGroups = filter(s.rep.FileGroups)
	return &cp
}

// handleReport sends the current (live) report.
func (s *Server) handleReport(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != "ready" {
		writeError(w, http.StatusConflict, "scan not finished")
		return
	}
	live := s.liveReport()
	writeJSON(w, http.StatusOK, map[string]any{"report": live, "reclaimable": live.Reclaimable()})
}

// selectRequest is the body of POST /api/select.
type selectRequest struct {
	Strategy string   `json:"strategy"`
	Prefer   []string `json:"prefer"`
	Kinds    string   `json:"kinds"`
}

// handleSelect runs a keep rule ("Auto-select") and returns the IDs it
// would remove. Changes nothing.
func (s *Server) handleSelect(w http.ResponseWriter, r *http.Request) {
	var req selectRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad request: "+err.Error())
		return
	}
	if req.Kinds == "" {
		req.Kinds = "all"
	}
	s.mu.Lock()
	if s.state != "ready" {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "scan not finished")
		return
	}
	live := s.liveReport()
	s.mu.Unlock()
	ids, err := keep.Select(live, keep.Rule{Strategy: keep.Strategy(req.Strategy), Prefer: req.Prefer}, req.Kinds)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if ids == nil {
		ids = []int{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"remove": ids})
}

// cleanRequest is the body of POST /api/clean.
type cleanRequest struct {
	Remove  []int  `json:"remove"`
	Action  string `json:"action"`
	Apply   bool   `json:"apply"`
	Confirm string `json:"confirm"` // must be "DELETE" for an applied permanent delete
}

// handleClean previews (apply=false) or performs a cleanup via clean.Run.
// Only one cleanup may run at a time.
func (s *Server) handleClean(w http.ResponseWriter, r *http.Request) {
	var req cleanRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad request: "+err.Error())
		return
	}
	action := clean.Action(req.Action)
	confirmed := req.Confirm == "DELETE"
	if req.Apply && action == clean.Delete && !confirmed {
		writeError(w, http.StatusBadRequest, "type DELETE to confirm permanent deletion")
		return
	}

	s.mu.Lock()
	if s.state != "ready" || s.busy {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "busy - wait for the current scan or cleanup to finish")
		return
	}
	s.busy = true
	live := s.liveReport()
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.busy = false
		s.mu.Unlock()
	}()

	// r.Context() is cancelled if the browser tab is closed mid-run, which
	// makes clean.Run stop before the next item.
	res, err := clean.Run(r.Context(), live, req.Remove, clean.Options{
		Action: action, Apply: req.Apply, ConfirmPermanent: confirmed,
		LogPath: s.cfg.LogPath, TrashFunc: s.cfg.TrashFunc,
	})
	if res == nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Apply {
		s.markRemoved(res)
	}
	out := map[string]any{"result": res}
	if err != nil && !errors.Is(err, context.Canceled) {
		out["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, out)
}

// markRemoved records which report entries are gone after a cleanup: the
// removed copies themselves, plus anything inside a removed folder.
func (s *Server) markRemoved(res *clean.Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var folders []string
	for _, o := range res.Outcomes {
		if o.Status == clean.Removed {
			s.removed[o.EntryID] = true
			if o.Kind == report.KindFolder {
				folders = append(folders, o.Path)
			}
		}
	}
	for _, g := range s.rep.Groups() {
		for _, e := range g.Entries {
			for _, f := range folders {
				if pathutil.IsUnder(e.Path, f) {
					s.removed[e.ID] = true
				}
			}
		}
	}
}

// handleShutdown lets the page's Quit button stop the server.
func (s *Server) handleShutdown(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	s.quitOnce.Do(func() { close(s.quit) })
}
