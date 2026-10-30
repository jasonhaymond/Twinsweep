// Browser-app endpoints that aren't background jobs: the folder picker,
// reading/exporting/loading reports, Auto-select, and the action log viewer.
package web

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"twinsweep/internal/clean"
	"twinsweep/internal/keep"
	"twinsweep/internal/report"
)

// place is one entry in the folder picker: a starting point (drive, home)
// or a subfolder.
type place struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// maxBrowseEntries caps one folder listing so a folder with a million
// subfolders can't produce an enormous response.
const maxBrowseEntries = 5000

// handleBrowse lists the subfolders of ?path= for the folder picker, or the
// starting points (drives, home) when path is empty (GET /api/browse).
//
// Security: this exposes folder NAMES only (never file contents) to the
// signed-in session, i.e. to the same user who could open File Explorer.
// It's read-only and runs with that user's own permissions, so it can't
// reveal anything they couldn't already see. Symlinks/junctions are not
// listed, matching the scanner, which never follows them.
func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if p == "" {
		writeJSON(w, http.StatusOK, map[string]any{"path": "", "parent": nil, "dirs": places()})
		return
	}
	if !filepath.IsAbs(p) {
		writeError(w, http.StatusBadRequest, "path must be a full path")
		return
	}
	// Accepted risk (approved 2026-10-30): no base-folder confinement on
	// purpose. The picker exists to choose scan folders anywhere the user
	// can open, so Clean here only normalises the path; it is not a
	// traversal guard. Exposure is limited to folder names, for the signed-in
	// session, under the user's own OS permissions (see function comment).
	// nosemgrep: go.lang.security.filepath-clean-misuse.filepath-clean-misuse
	p = filepath.Clean(p)
	list, err := os.ReadDir(p)
	if err != nil {
		writeError(w, http.StatusBadRequest, "can't open this folder: "+plainErr(err))
		return
	}
	dirs := []place{}
	for _, e := range list {
		// Exact type check: a plain folder, not a link to one.
		if e.Type() == fs.ModeDir {
			dirs = append(dirs, place{Name: e.Name(), Path: filepath.Join(p, e.Name())})
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return strings.ToLower(dirs[i].Name) < strings.ToLower(dirs[j].Name) })
	truncated := len(dirs) > maxBrowseEntries
	if truncated {
		dirs = dirs[:maxBrowseEntries]
	}
	// Parent of a drive/filesystem root is the "starting points" list ("").
	parent := filepath.Dir(p)
	if parent == p {
		parent = ""
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": p, "parent": parent, "dirs": dirs, "truncated": truncated})
}

// plainErr strips Go's "open <path>: " prefix from an OS error so the
// message reads naturally ("Access is denied.").
func plainErr(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// handleReport sends the current (live) report (GET /api/report).
func (s *Server) handleReport(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rep == nil {
		writeError(w, http.StatusConflict, "no report yet - run a scan or load a report")
		return
	}
	live := s.liveReport()
	writeJSON(w, http.StatusOK, map[string]any{"report": live, "reclaimable": live.Reclaimable(), "version": s.repVersion})
}

// handleExport downloads the current report as JSON (loadable later, by the
// CLI or this page) or CSV (for spreadsheets) (GET /api/report/export).
// Copies removed this session are left out. The CSV writer neutralizes
// spreadsheet formula injection (see report.WriteCSV).
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	if format != "json" && format != "csv" {
		writeError(w, http.StatusBadRequest, "format must be json or csv")
		return
	}
	s.mu.Lock()
	if s.rep == nil {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "no report to export")
		return
	}
	live := s.liveReport()
	s.mu.Unlock()

	var buf bytes.Buffer
	ct := "application/json; charset=utf-8"
	if format == "csv" {
		ct = "text/csv; charset=utf-8"
		if err := report.WriteCSV(&buf, live); err != nil {
			writeError(w, http.StatusInternalServerError, "export failed")
			return
		}
	} else {
		enc := json.NewEncoder(&buf)
		enc.SetIndent("", "  ")
		if err := enc.Encode(live); err != nil {
			writeError(w, http.StatusInternalServerError, "export failed")
			return
		}
	}
	name := fmt.Sprintf("twinsweep-report-%s.%s", time.Now().Format("20060102-150405"), format)
	w.Header().Set("Content-Type", ct)
	// "attachment" makes the browser save the file instead of displaying it.
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	_, _ = w.Write(buf.Bytes())
}

// handleLoad replaces the current report with an uploaded one (POST
// /api/report/load; body = the report file's JSON). The upload goes through
// the same validation as `--report` (report.Parse). Loading a report can't
// harm anything by itself: cleanup re-verifies every copy on disk first.
func (s *Server) handleLoad(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "report is too large or could not be read")
		return
	}
	rep, err := report.Parse(data)
	if err != nil {
		writeError(w, http.StatusBadRequest, "can't load this file: "+err.Error())
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if b := s.busyLocked(); b != "" {
		writeError(w, http.StatusConflict, "busy - "+b)
		return
	}
	s.rep, s.removed, s.scanErr = rep, map[int]bool{}, ""
	s.lastScan = requestFromReport(rep)
	s.repVersion++
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "groups": len(rep.FileGroups) + len(rep.FolderGroups)})
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
	if s.rep == nil {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "no report loaded")
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

// handleLog returns the newest ?lines= (default 200, max 2000) action-log
// records, newest first (GET /api/log). Unparseable lines are skipped.
func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	n := 200
	if v, err := strconv.Atoi(r.URL.Query().Get("lines")); err == nil && v > 0 && v <= 2000 {
		n = v
	}
	path := s.cfg.LogPath
	if path == "" {
		path = clean.DefaultLogPath()
	}
	f, err := os.Open(path) // #nosec G304 -- the app's own action log (default or --log)
	if errors.Is(err, fs.ErrNotExist) {
		writeJSON(w, http.StatusOK, map[string]any{"path": path, "entries": []any{}})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "can't read the action log: "+plainErr(err))
		return
	}
	defer f.Close()
	// The log is capped at ~10 MiB by rotation, so reading it line by line
	// and keeping the last n is cheap.
	var lines []json.RawMessage
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		b := sc.Bytes()
		if !json.Valid(b) {
			continue
		}
		lines = append(lines, append(json.RawMessage(nil), b...))
		if len(lines) > n {
			lines = lines[1:]
		}
	}
	// Reverse so the newest action is first.
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	if lines == nil {
		lines = []json.RawMessage{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": path, "entries": lines})
}
