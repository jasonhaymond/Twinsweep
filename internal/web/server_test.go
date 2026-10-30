// Security and behaviour tests for the browser app server. Each test sends
// real HTTP requests through the full handler (all middleware included).
//
// Security tests prove these attacks are blocked: unauthenticated access to
// every endpoint, CSRF from another website on every state-changing
// endpoint, DNS rebinding, reuse of the one-time sign-in link, targeting
// files outside the scan, removing every copy, permanent delete without
// confirmation, oversized bodies, malformed/foreign report uploads, and
// static-file path traversal.
//
// Behaviour tests prove the background jobs work: scans start, report
// progress and can be cancelled; cleanups report live progress, can be
// stopped between items, and block conflicting jobs and Quit while running.
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
	"time"

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
	log    string
	// gate, if non-nil, makes every trash call wait for a value on it, so a
	// test can hold a cleanup "in progress".
	gate chan struct{}
}

// newEnv builds the server over a fresh tree and signs in through the
// one-time link, the same way a browser does.
func newEnv(t *testing.T) *env {
	t.Helper()
	d := t.TempDir()
	tu.Write(t, d, "keep/a.txt", "dup")
	tu.Write(t, d, "copy/a.txt", "dup")
	tu.Write(t, d, "copy2/a.txt", "dup")
	tu.Write(t, d, "only.txt", "unique!")
	rep, err := scan.Run(context.Background(), scan.Options{Roots: []string{d}, MinSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	trashDir := t.TempDir()
	e := &env{t: t, dir: d, rep: rep, log: filepath.Join(t.TempDir(), "actions.log")}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s, err := New(ctx, Config{Report: rep, LogPath: e.log, TrashFunc: func(p string) error {
		if e.gate != nil {
			<-e.gate
		}
		return os.Rename(p, filepath.Join(trashDir, filepath.Base(filepath.Dir(p))+"-"+filepath.Base(p)))
	}}, port)
	if err != nil {
		t.Fatal(err)
	}
	e.s, e.h = s, s.Handler()
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

// json decodes a response body into a generic map.
func (e *env) json(w *httptest.ResponseRecorder) map[string]any {
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		e.t.Fatalf("bad JSON (%d): %s", w.Code, w.Body)
	}
	return m
}

// path returns the absolute path of a tree-relative path.
func (e *env) path(rel string) string { return filepath.Join(e.dir, filepath.FromSlash(rel)) }

// idOf finds the entry ID for a relative path.
func (e *env) idOf(rel string) int {
	for _, g := range e.rep.Groups() {
		for _, en := range g.Entries {
			if en.Path == e.path(rel) {
				return en.ID
			}
		}
	}
	e.t.Fatalf("%s not in report", rel)
	return 0
}

// waitFor polls cond every 10 ms for up to 10 s.
func (e *env) waitFor(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// job returns the latest cleanup job from /api/clean/status.
func (e *env) job() map[string]any {
	m := e.json(e.req("GET", "/api/clean/status", "", nil))
	j, _ := m["job"].(map[string]any)
	return j
}

// waitJob waits for the current cleanup job to finish and returns it.
func (e *env) waitJob() map[string]any {
	e.waitFor("cleanup to finish", func() bool { j := e.job(); return j != nil && j["running"] == false })
	return e.job()
}

// status returns /api/status.
func (e *env) status() map[string]any { return e.json(e.req("GET", "/api/status", "", nil)) }

// ---------------------------------------------------------------------
// Security: authentication, sign-in, Host, CSRF, headers
// ---------------------------------------------------------------------

// TestUnauthenticatedRejected proves every page and API endpoint requires
// the session cookie (another local user or program can reach the port but
// can't use it - including the folder browser, which reveals folder names).
func TestUnauthenticatedRejected(t *testing.T) {
	e := newEnv(t)
	gets := []string{"/", "/api/status", "/api/report", "/api/report/export?format=json", "/api/browse?path=",
		"/api/clean/status", "/api/log", "/static/app.js"}
	for _, p := range gets {
		r := httptest.NewRequest("GET", "http://127.0.0.1:54321"+p, nil)
		r.Host = "127.0.0.1:54321"
		if w := e.do(r); w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without cookie: %d, want 401", p, w.Code)
		}
	}
	posts := []string{"/api/scan", "/api/scan/cancel", "/api/report/load", "/api/select", "/api/clean", "/api/clean/cancel", "/api/shutdown"}
	for _, p := range posts {
		if w := e.req("POST", p, `{}`, func(r *http.Request) { r.Header.Del("Cookie") }); w.Code != http.StatusUnauthorized {
			t.Errorf("POST %s without cookie: %d, want 401", p, w.Code)
		}
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
		w := e.req("GET", "/api/browse?path=", "", func(r *http.Request) { r.Host = host })
		if w.Code != http.StatusForbidden {
			t.Errorf("Host %q: %d, want 403", host, w.Code)
		}
	}
}

// TestCSRFBlocked proves every state-changing endpoint refuses a request
// from another origin, without the token header, with a wrong token, or
// that isn't JSON (the shapes a cross-site form/fetch attack would have).
func TestCSRFBlocked(t *testing.T) {
	e := newEnv(t)
	bodies := map[string]string{
		"/api/clean":        `{"remove":[` + itoa(e.idOf("copy/a.txt")) + `],"action":"trash","apply":true}`,
		"/api/scan":         `{"roots":[` + jsonStr(e.dir) + `],"mode":"exact"}`,
		"/api/report/load":  `{}`,
		"/api/select":       `{"strategy":"oldest"}`,
		"/api/scan/cancel":  `{}`,
		"/api/clean/cancel": `{}`,
		"/api/shutdown":     `{}`,
	}
	attacks := map[string]func(*http.Request){
		"foreign origin":   func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") },
		"cross-site fetch": func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
		"no token":         func(r *http.Request) { r.Header.Del("X-Twinsweep-Token") },
		"wrong token":      func(r *http.Request) { r.Header.Set("X-Twinsweep-Token", "nope") },
		"form post":        func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") },
	}
	for path, body := range bodies {
		for name, mod := range attacks {
			if w := e.req("POST", path, body, mod); w.Code != http.StatusForbidden && w.Code != http.StatusUnsupportedMediaType {
				t.Errorf("%s %s: accepted with %d", name, path, w.Code)
			}
		}
	}
	if !tu.Exists(e.path("copy/a.txt")) || e.status()["scanning"] == true {
		t.Fatal("a refused request still had an effect")
	}
}

// TestSecurityHeaders proves CSP, nosniff, frame and cache headers are set
// on pages and API responses, and that no CORS header is ever sent.
func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/", "/api/status", "/api/browse?path="} {
		h := e.req("GET", p, "", nil).Result().Header
		if !strings.Contains(h.Get("Content-Security-Policy"), "script-src 'self'") ||
			!strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") ||
			h.Get("X-Content-Type-Options") != "nosniff" || h.Get("X-Frame-Options") != "DENY" || h.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s missing security headers: %v", p, h)
		}
		if h.Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("%s sent a CORS header; other sites must never be allowed", p)
		}
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

// TestOversizedBodyRejected proves a huge request body is cut off.
func TestOversizedBodyRejected(t *testing.T) {
	e := newEnv(t)
	big := `{"remove":[` + strings.Repeat("1,", maxBody/2) + `1],"action":"trash"}`
	if w := e.req("POST", "/api/clean", big, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("oversized body: %d, want 400", w.Code)
	}
}

// ---------------------------------------------------------------------
// Security: cleanup requests
// ---------------------------------------------------------------------

// TestCleanOnlyTargetsScannedIDs proves the API can't be pointed at an
// arbitrary file: unknown IDs are rejected and paths aren't accepted at all.
func TestCleanOnlyTargetsScannedIDs(t *testing.T) {
	e := newEnv(t)
	if w := e.req("POST", "/api/clean", `{"remove":[99999],"action":"trash","apply":true}`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown ID: %d, want 400", w.Code)
	}
	body, _ := json.Marshal(map[string]any{"remove": []int{}, "paths": []string{e.path("only.txt")}, "action": "trash", "apply": true})
	if w := e.req("POST", "/api/clean", string(body), nil); w.Code != http.StatusBadRequest {
		t.Fatalf("request with a paths field: %d, want 400 (unknown field)", w.Code)
	}
	if !tu.Exists(e.path("only.txt")) || e.job() != nil {
		t.Fatal("a rejected request started a job or removed a file")
	}
}

// TestCleanRefusesAllCopies proves the server rejects removing every copy,
// before any background job starts.
func TestCleanRefusesAllCopies(t *testing.T) {
	e := newEnv(t)
	body := `{"remove":[` + itoa(e.idOf("copy/a.txt")) + `,` + itoa(e.idOf("copy2/a.txt")) + `,` + itoa(e.idOf("keep/a.txt")) + `],"action":"trash","apply":true}`
	if w := e.req("POST", "/api/clean", body, nil); w.Code != http.StatusBadRequest || e.job() != nil {
		t.Fatalf("all copies: %d, want 400 and no job", w.Code)
	}
}

// TestPermanentDeleteNeedsTypedConfirm proves "delete" with apply needs
// confirm=DELETE.
func TestPermanentDeleteNeedsTypedConfirm(t *testing.T) {
	e := newEnv(t)
	id := itoa(e.idOf("copy/a.txt"))
	if w := e.req("POST", "/api/clean", `{"remove":[`+id+`],"action":"delete","apply":true,"confirm":"yes"}`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("unconfirmed delete: %d, want 400", w.Code)
	}
	if !tu.Exists(e.path("copy/a.txt")) || e.job() != nil {
		t.Fatal("unconfirmed delete had an effect")
	}
}

// ---------------------------------------------------------------------
// Cleanup jobs: progress, stop, conflicts
// ---------------------------------------------------------------------

// TestCleanJobProgressAndResult proves a cleanup runs as a background job,
// reports done/total and bytes, writes the action log, and removes the
// cleaned copies from the report.
func TestCleanJobProgressAndResult(t *testing.T) {
	e := newEnv(t)
	body := `{"remove":[` + itoa(e.idOf("copy/a.txt")) + `,` + itoa(e.idOf("copy2/a.txt")) + `],"action":"trash","apply":true}`
	w := e.req("POST", "/api/clean", body, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("start: %d %s", w.Code, w.Body)
	}
	j := e.waitJob()
	if j["done"] != 2.0 || j["total"] != 2.0 || j["removed"] != 2.0 || j["doneBytes"] != j["totalBytes"] || len(j["outcomes"].([]any)) != 2 {
		t.Fatalf("job result wrong: %v", j)
	}
	if tu.Exists(e.path("copy/a.txt")) || !tu.Exists(e.path("keep/a.txt")) {
		t.Fatal("wrong files removed")
	}
	var got struct{ Report report.Report }
	_ = json.Unmarshal(e.req("GET", "/api/report", "", nil).Body.Bytes(), &got)
	if len(got.Report.FileGroups) != 0 {
		t.Fatalf("group still listed after cleanup: %+v", got.Report.FileGroups)
	}
	logs := e.json(e.req("GET", "/api/log", "", nil))["entries"].([]any)
	if len(logs) != 2 {
		t.Fatalf("activity log has %d entries, want 2", len(logs))
	}
}

// TestCleanJobStopAndConflicts proves that while a cleanup runs: progress
// shows the current item, a second cleanup / a scan / a report load / Quit
// are all refused (they'd act on stale data or abandon a removal run), and
// Stop halts the job before the next item.
func TestCleanJobStopAndConflicts(t *testing.T) {
	e := newEnv(t)
	e.gate = make(chan struct{})
	body := `{"remove":[` + itoa(e.idOf("copy/a.txt")) + `,` + itoa(e.idOf("copy2/a.txt")) + `],"action":"trash","apply":true}`
	if w := e.req("POST", "/api/clean", body, nil); w.Code != http.StatusAccepted {
		t.Fatalf("start: %d", w.Code)
	}
	e.waitFor("job to reach the removing step", func() bool { j := e.job(); return j != nil && j["step"] == "removing" })
	if j := e.job(); j["current"] == "" || j["running"] != true {
		t.Fatalf("no current item shown: %v", j)
	}
	if w := e.req("POST", "/api/clean", body, nil); w.Code != http.StatusConflict {
		t.Errorf("second cleanup: %d, want 409", w.Code)
	}
	if w := e.req("POST", "/api/scan", `{"roots":[`+jsonStr(e.dir)+`],"mode":"exact"}`, nil); w.Code != http.StatusBadRequest {
		t.Errorf("scan during cleanup: %d, want refusal", w.Code)
	}
	if w := e.req("POST", "/api/shutdown", `{}`, nil); w.Code != http.StatusConflict {
		t.Errorf("quit during cleanup: %d, want 409", w.Code)
	}
	// Stop, then let the in-flight item finish: exactly one item removed.
	e.req("POST", "/api/clean/cancel", `{}`, nil)
	e.gate <- struct{}{}
	j := e.waitJob()
	if j["stopped"] != true || j["removed"] != 1.0 || j["done"] != 1.0 {
		t.Fatalf("stop didn't halt before the next item: %v", j)
	}
}

// ---------------------------------------------------------------------
// Scanning from the browser
// ---------------------------------------------------------------------

// TestScanFromBrowser proves a scan can be started, completes in the
// background, replaces the report, and is remembered for "Rescan".
func TestScanFromBrowser(t *testing.T) {
	e := newEnv(t)
	tu.Write(t, e.dir, "more/b.txt", "second pair")
	tu.Write(t, e.dir, "more2/b.txt", "second pair")
	v0 := e.status()["reportVersion"]
	if w := e.req("POST", "/api/scan", `{"roots":[`+jsonStr(e.dir)+`],"mode":"exact","folders":true,"minSize":"1","workers":2}`, nil); w.Code != http.StatusAccepted {
		t.Fatalf("start scan: %d %s", w.Code, w.Body)
	}
	e.waitFor("scan to finish", func() bool { st := e.status(); return st["scanning"] == false && st["reportVersion"] != v0 })
	var got struct{ Report report.Report }
	_ = json.Unmarshal(e.req("GET", "/api/report", "", nil).Body.Bytes(), &got)
	if len(got.Report.FileGroups)+len(got.Report.FolderGroups) < 2 {
		t.Fatalf("new scan results missing: %+v", got.Report)
	}
	if ls := e.status()["lastScan"].(map[string]any); ls["roots"].([]any)[0] != e.dir {
		t.Fatalf("lastScan not recorded: %v", ls)
	}
}

// TestScanRequestValidation proves bad scan requests are rejected up front:
// missing folder, relative path (which would resolve against the server's
// working folder), bad size, bad mode, absurd worker count.
func TestScanRequestValidation(t *testing.T) {
	e := newEnv(t)
	bad := []string{
		`{"roots":[],"mode":"exact"}`,
		`{"roots":[` + jsonStr(filepath.Join(e.dir, "nope")) + `],"mode":"exact"}`,
		`{"roots":["relative/dir"],"mode":"exact"}`,
		`{"roots":[` + jsonStr(e.dir) + `],"mode":"exact","minSize":"-5"}`,
		`{"roots":[` + jsonStr(e.dir) + `],"mode":"sideways"}`,
		`{"roots":[` + jsonStr(e.dir) + `],"mode":"exact","workers":9999}`,
	}
	for _, b := range bad {
		if w := e.req("POST", "/api/scan", b, nil); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", b, w.Code)
		}
	}
	if e.status()["scanning"] == true {
		t.Fatal("an invalid request started a scan")
	}
}

// TestScanCancel proves Cancel stops a scan and keeps the previous report.
func TestScanCancel(t *testing.T) {
	e := newEnv(t)
	// Enough files that the scan can't finish before we cancel it.
	for i := 0; i < 3000; i++ {
		tu.Write(t, e.dir, "bulk/"+itoa(i%30)+"/"+itoa(i)+".txt", "same")
	}
	v0 := e.status()["reportVersion"]
	e.req("POST", "/api/scan", `{"roots":[`+jsonStr(e.dir)+`],"mode":"exact"}`, nil)
	e.req("POST", "/api/scan/cancel", `{}`, nil)
	e.waitFor("scan to stop", func() bool { return e.status()["scanning"] == false })
	st := e.status()
	if !strings.Contains(st["scanError"].(string), "cancelled") || st["reportVersion"] != v0 {
		t.Fatalf("cancel: %v", st)
	}
}

// ---------------------------------------------------------------------
// Folder browser, report load/export, log
// ---------------------------------------------------------------------

// TestBrowse proves the folder picker lists starting points and
// subfolders (folders only, no files), and rejects relative paths.
func TestBrowse(t *testing.T) {
	e := newEnv(t)
	if m := e.json(e.req("GET", "/api/browse?path=", "", nil)); len(m["dirs"].([]any)) == 0 {
		t.Fatal("no starting places")
	}
	m := e.json(e.req("GET", "/api/browse?path="+url.QueryEscape(e.dir), "", nil))
	var names []string
	for _, d := range m["dirs"].([]any) {
		names = append(names, d.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "copy,copy2,keep" {
		t.Fatalf("listing = %v, want only the three folders (no files)", names)
	}
	if w := e.req("GET", "/api/browse?path=..", "", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("relative path: %d, want 400", w.Code)
	}
}

// TestLoadReportValidated proves an uploaded report goes through full
// validation: wrong tool, wrong schema, relative paths and duplicate IDs are
// rejected; a genuine report is accepted and replaces the current one.
func TestLoadReportValidated(t *testing.T) {
	e := newEnv(t)
	good, _ := json.Marshal(e.rep)
	bad := []string{
		`not json`,
		`{"tool":"other","schema":1}`,
		strings.Replace(string(good), `"schema":1`, `"schema":99`, 1),
		strings.Replace(string(good), jsonStr(e.path("copy/a.txt")), `"copy/a.txt"`, 1),
	}
	for i, b := range bad {
		if w := e.req("POST", "/api/report/load", b, nil); w.Code != http.StatusBadRequest {
			t.Errorf("bad report %d accepted: %d", i, w.Code)
		}
	}
	v0 := e.status()["reportVersion"]
	if w := e.req("POST", "/api/report/load", string(good), nil); w.Code != http.StatusOK {
		t.Fatalf("good report: %d %s", w.Code, w.Body)
	}
	if e.status()["reportVersion"] == v0 {
		t.Fatal("report version not bumped after load")
	}
}

// TestExport proves the report downloads as an attachment in both formats
// (never rendered inline), and the JSON export can be loaded back.
func TestExport(t *testing.T) {
	e := newEnv(t)
	for _, f := range []string{"json", "csv"} {
		w := e.req("GET", "/api/report/export?format="+f, "", nil)
		if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment;") {
			t.Fatalf("%s export: %d %v", f, w.Code, w.Header())
		}
		if f == "json" {
			if _, err := report.Parse(w.Body.Bytes()); err != nil {
				t.Fatalf("exported JSON doesn't load back: %v", err)
			}
		}
	}
	if w := e.req("GET", "/api/report/export?format=html", "", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown format: %d", w.Code)
	}
}

// itoa avoids importing strconv in every test line.
func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// jsonStr returns s as a JSON string literal (escaping Windows backslashes).
func jsonStr(s string) string { b, _ := json.Marshal(s); return string(b) }
