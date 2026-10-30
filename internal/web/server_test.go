// Security tests for the review server. Each test sends real HTTP requests
// through the full handler (all middleware included) and proves one attack
// is blocked: unauthenticated access, CSRF from another website, DNS
// rebinding, reuse of the one-time sign-in link, targeting files outside the
// scan, removing every copy, and permanent delete without confirmation.
package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"twinsweep/internal/report"
	"twinsweep/internal/scan"
	tu "twinsweep/internal/testutil"
)

const port = 54321

// env is a server over a scanned temp folder with one duplicate pair.
type env struct {
	t      *testing.T
	s      *Server
	h      http.Handler
	dir    string
	cookie *http.Cookie
	rep    *report.Report
}

// newEnv builds the server and signs in through the one-time link, the same
// way a browser does.
func newEnv(t *testing.T) *env {
	t.Helper()
	d := t.TempDir()
	tu.Write(t, d, "keep/a.txt", "dup")
	tu.Write(t, d, "copy/a.txt", "dup")
	tu.Write(t, d, "only.txt", "unique!")
	rep, err := scan.Run(context.Background(), scan.Options{Roots: []string{d}, MinSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	trashDir := t.TempDir()
	s, err := New(Config{Report: rep, LogPath: filepath.Join(t.TempDir(), "log"), TrashFunc: func(p string) error {
		return os.Rename(p, filepath.Join(trashDir, filepath.Base(p)))
	}}, port)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, s: s, h: s.Handler(), dir: d, rep: rep}
	u, _ := url.Parse(s.SignInURL())
	w := e.do(&http.Request{Method: "GET", URL: u, Host: u.Host, Header: http.Header{}})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("sign-in: status %d", w.Code)
	}
	for _, c := range w.Result().Cookies() {
		e.cookie = c
	}
	if e.cookie == nil || !e.cookie.HttpOnly || e.cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie missing or lacks HttpOnly/SameSite=Strict: %+v", e.cookie)
	}
	return e
}

// do sends a request through the full handler.
func (e *env) do(r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

// req builds a request with the given method/path/body. By default it looks
// like a legitimate request from our own page (right Host, cookie, Origin,
// token header, JSON); mod can strip or alter any of that.
func (e *env) req(method, path, body string, mod func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://127.0.0.1:54321"+path, strings.NewReader(body))
	r.Host = "127.0.0.1:54321"
	r.AddCookie(e.cookie)
	if method == "POST" {
		r.Header.Set("Origin", "http://127.0.0.1:54321")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set("X-Twinsweep-Token", e.s.session)
		r.Header.Set("Content-Type", "application/json")
	}
	if mod != nil {
		mod(r)
	}
	return e.do(r)
}

// idOf finds the entry ID for a relative path.
func (e *env) idOf(rel string) int {
	for _, g := range e.rep.Groups() {
		for _, en := range g.Entries {
			if en.Path == filepath.Join(e.dir, filepath.FromSlash(rel)) {
				return en.ID
			}
		}
	}
	e.t.Fatalf("%s not in report", rel)
	return 0
}

// TestUnauthenticatedRejected proves every page and API endpoint requires
// the session cookie (another local user or program can reach the port but
// can't use it).
func TestUnauthenticatedRejected(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/", "/api/status", "/api/report", "/static/app.js"} {
		r := httptest.NewRequest("GET", "http://127.0.0.1:54321"+p, nil)
		r.Host = "127.0.0.1:54321"
		if w := e.do(r); w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without cookie: %d, want 401", p, w.Code)
		}
	}
	w := e.req("POST", "/api/clean", `{"remove":[1],"action":"trash","apply":true}`, func(r *http.Request) {
		r.Header.Del("Cookie")
	})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("POST without cookie: %d, want 401", w.Code)
	}
	if w := e.req("GET", "/api/status", "", func(r *http.Request) {
		r.Header.Del("Cookie")
		r.AddCookie(&http.Cookie{Name: e.cookie.Name, Value: strings.Repeat("0", 64)})
	}); w.Code != http.StatusUnauthorized {
		t.Fatalf("forged cookie accepted: %d", w.Code)
	}
}

// TestSignInLinkIsOneTime proves the printed link stops working after first
// use, so a link seen in a process list or shell history is useless.
func TestSignInLinkIsOneTime(t *testing.T) {
	e := newEnv(t) // already used the link once
	u, _ := url.Parse(e.s.SignInURL())
	if !strings.HasSuffix(u.RawQuery, "t=") {
		t.Fatalf("bootstrap not cleared: %s", u.RawQuery)
	}
	r := httptest.NewRequest("GET", "http://127.0.0.1:54321/?t=anything", nil)
	r.Host = "127.0.0.1:54321"
	if w := e.do(r); w.Code != http.StatusUnauthorized || len(w.Result().Cookies()) != 0 {
		t.Fatalf("reused/guessed link: %d, cookies %v", w.Code, w.Result().Cookies())
	}
}

// TestDNSRebindingBlocked proves requests with a foreign Host header (a
// malicious domain resolved to 127.0.0.1) are refused even with a cookie.
func TestDNSRebindingBlocked(t *testing.T) {
	e := newEnv(t)
	for _, host := range []string{"evil.example:54321", "127.0.0.1:1", "127.0.0.1"} {
		w := e.req("GET", "/api/report", "", func(r *http.Request) { r.Host = host })
		if w.Code != http.StatusForbidden {
			t.Errorf("Host %q: %d, want 403", host, w.Code)
		}
	}
}

// TestCSRFBlocked proves a change request is refused if it comes from
// another origin, lacks the token header, has the wrong token, or isn't
// JSON (the shapes a cross-site form/fetch attack would have).
func TestCSRFBlocked(t *testing.T) {
	e := newEnv(t)
	body := `{"remove":[` + itoa(e.idOf("copy/a.txt")) + `],"action":"trash","apply":true}`
	cases := map[string]func(*http.Request){
		"foreign origin":   func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") },
		"cross-site fetch": func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
		"no token":         func(r *http.Request) { r.Header.Del("X-Twinsweep-Token") },
		"wrong token":      func(r *http.Request) { r.Header.Set("X-Twinsweep-Token", "nope") },
		"form post":        func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") },
	}
	for name, mod := range cases {
		if w := e.req("POST", "/api/clean", body, mod); w.Code < 400 {
			t.Errorf("%s: accepted with %d", name, w.Code)
		}
	}
	if !tu.Exists(filepath.Join(e.dir, "copy", "a.txt")) {
		t.Fatal("a refused request still removed a file")
	}
}

// TestSecurityHeaders proves CSP, nosniff, frame and cache headers are set.
func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t)
	w := e.req("GET", "/", "", nil)
	h := w.Result().Header
	if !strings.Contains(h.Get("Content-Security-Policy"), "script-src 'self'") ||
		!strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") ||
		h.Get("X-Content-Type-Options") != "nosniff" || h.Get("X-Frame-Options") != "DENY" || h.Get("Cache-Control") != "no-store" {
		t.Fatalf("missing security headers: %v", h)
	}
	if h.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("CORS header present; other sites must never be allowed")
	}
}

// TestStaticNoTraversal proves only the two known static files are served:
// no directory listing and no ../ escape to the embedded template or disk.
func TestStaticNoTraversal(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/static/index.html", "/static/", "/static/..%2fserver.go", "/static/%2e%2e/go.mod"} {
		if w := e.req("GET", p, "", nil); w.Code == http.StatusOK {
			t.Errorf("GET %s served (%d)", p, w.Code)
		}
	}
	if w := e.req("GET", "/static/app.js", "", nil); w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/javascript") {
		t.Fatalf("app.js: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
}

// TestCleanOnlyTargetsScannedIDs proves the API can't be pointed at an
// arbitrary file: unknown IDs are rejected and paths aren't accepted at all.
func TestCleanOnlyTargetsScannedIDs(t *testing.T) {
	e := newEnv(t)
	if w := e.req("POST", "/api/clean", `{"remove":[99999],"action":"trash","apply":true}`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown ID: %d, want 400", w.Code)
	}
	only := filepath.Join(e.dir, "only.txt")
	body, _ := json.Marshal(map[string]any{"remove": []int{}, "paths": []string{only}, "action": "trash", "apply": true})
	if w := e.req("POST", "/api/clean", string(body), nil); w.Code != http.StatusBadRequest {
		t.Fatalf("request with a paths field: %d, want 400 (unknown field)", w.Code)
	}
	if !tu.Exists(only) {
		t.Fatal("unique file removed")
	}
}

// TestCleanRefusesAllCopies proves the server rejects removing every copy.
func TestCleanRefusesAllCopies(t *testing.T) {
	e := newEnv(t)
	body := `{"remove":[` + itoa(e.idOf("copy/a.txt")) + `,` + itoa(e.idOf("keep/a.txt")) + `],"action":"trash","apply":true}`
	if w := e.req("POST", "/api/clean", body, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("all copies: %d, want 400", w.Code)
	}
}

// TestPermanentDeleteNeedsTypedConfirm proves "delete" with apply needs
// confirm=DELETE, and that the normal trash flow works end to end.
func TestPermanentDeleteNeedsTypedConfirm(t *testing.T) {
	e := newEnv(t)
	id := itoa(e.idOf("copy/a.txt"))
	if w := e.req("POST", "/api/clean", `{"remove":[`+id+`],"action":"delete","apply":true,"confirm":"yes"}`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("unconfirmed delete: %d, want 400", w.Code)
	}
	w := e.req("POST", "/api/clean", `{"remove":[`+id+`],"action":"trash","apply":true}`, nil)
	if w.Code != http.StatusOK || tu.Exists(filepath.Join(e.dir, "copy", "a.txt")) {
		t.Fatalf("trash flow failed: %d %s", w.Code, w.Body)
	}
	// The removed copy's group now has one copy and disappears from the report.
	var got struct{ Report report.Report }
	_ = json.Unmarshal(e.req("GET", "/api/report", "", nil).Body.Bytes(), &got)
	if len(got.Report.FileGroups) != 0 {
		t.Fatalf("group still listed after cleanup: %+v", got.Report.FileGroups)
	}
}

// TestOversizedBodyRejected proves a huge request body is cut off.
func TestOversizedBodyRejected(t *testing.T) {
	e := newEnv(t)
	big := `{"remove":[` + strings.Repeat("1,", maxBody/2) + `1],"action":"trash"}`
	if w := e.req("POST", "/api/clean", big, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("oversized body: %d, want 400", w.Code)
	}
}

// itoa avoids importing strconv in every test line.
func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }
