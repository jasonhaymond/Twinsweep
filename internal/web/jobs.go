// Background jobs for the browser app: scanning and cleanup.
//
// Both can take minutes, longer than a browser request should wait, so each
// runs in a goroutine (Go's lightweight background thread). The page starts a
// job with a POST, then polls /api/status (scan) or /api/clean/status
// (cleanup) about once a second to draw progress. Only one job of either kind
// runs at a time: scanning while removing (or removing while the report is
// being replaced) could act on stale information.
//
// Cancellation: each job has its own context (Go's cancel signal) derived
// from the server's. "Cancel"/"Stop" cancels it; a scan stops promptly, a
// cleanup stops before its next item - never halfway through removing one.
package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"twinsweep/internal/clean"
	"twinsweep/internal/pathutil"
	"twinsweep/internal/report"
	"twinsweep/internal/scan"
)

// Limits on a scan request, so a malformed request can't ask for absurd
// amounts of work or memory.
const (
	maxRoots    = 100
	maxExcludes = 500
	maxWorkers  = 64
)

// scanRequest is the body of POST /api/scan, and also what /api/status
// reports as "lastScan" so the form can be prefilled and "Rescan" can repeat it.
type scanRequest struct {
	Roots           []string `json:"roots"`
	Mode            string   `json:"mode"`    // "exact" or "fast"
	Folders         bool     `json:"folders"` // also find duplicate folders
	MinSize         string   `json:"minSize"` // human size, e.g. "1", "4K", "10M"
	Excludes        []string `json:"excludes"`
	DefaultExcludes bool     `json:"defaultExcludes"` // skip OS trash/index folders
	Workers         int      `json:"workers"`
}

// options validates the request and converts it to scan.Options.
func (q scanRequest) options() (scan.Options, error) {
	if len(q.Roots) == 0 {
		return scan.Options{}, errors.New("add at least one folder to scan")
	}
	if len(q.Roots) > maxRoots || len(q.Excludes) > maxExcludes {
		return scan.Options{}, errors.New("too many folders or exclude patterns")
	}
	mode := report.Mode(q.Mode)
	if mode != report.ModeExact && mode != report.ModeFast {
		return scan.Options{}, fmt.Errorf("unknown mode %q", q.Mode)
	}
	minSize := int64(1)
	if q.MinSize != "" {
		n, err := report.ParseSize(q.MinSize)
		if err != nil {
			return scan.Options{}, err
		}
		minSize = n
	}
	workers := q.Workers
	if workers <= 0 {
		workers = 4
	}
	if workers > maxWorkers {
		return scan.Options{}, fmt.Errorf("parallel reads must be between 1 and %d", maxWorkers)
	}
	// Folder paths must be absolute: a relative path would be resolved
	// against whatever folder the server happened to be started in.
	for _, r := range q.Roots {
		if !filepath.IsAbs(r) {
			return scan.Options{}, fmt.Errorf("%q is not a full path (e.g. C:\\Photos or /home/me/Photos)", r)
		}
	}
	return scan.Options{
		Roots: q.Roots, Mode: mode, MinSize: minSize, Excludes: q.Excludes,
		DefaultExcludes: q.DefaultExcludes, Folders: q.Folders, Workers: workers,
	}, nil
}

// requestFromOptions turns CLI-supplied scan options into the form shape.
func requestFromOptions(o scan.Options) *scanRequest {
	roots := make([]string, 0, len(o.Roots))
	for _, r := range o.Roots {
		if a, err := filepath.Abs(r); err == nil {
			roots = append(roots, a)
		}
	}
	mode := string(o.Mode)
	if mode == "" {
		mode = string(report.ModeExact)
	}
	return &scanRequest{Roots: roots, Mode: mode, Folders: o.Folders, MinSize: strconv.FormatInt(o.MinSize, 10),
		Excludes: o.Excludes, DefaultExcludes: o.DefaultExcludes, Workers: o.Workers}
}

// requestFromReport recovers the scan settings recorded in a loaded report,
// so "Rescan" repeats the scan that produced it.
func requestFromReport(r *report.Report) *scanRequest {
	return &scanRequest{Roots: r.Roots, Mode: string(r.Mode), Folders: r.Folders,
		MinSize: strconv.FormatInt(r.MinSize, 10), Excludes: r.Excludes, DefaultExcludes: true, Workers: 4}
}

// busyLocked reports why a new job can't start right now ("" = free).
// Caller must hold s.mu.
func (s *Server) busyLocked() string {
	switch {
	case s.scanning:
		return "a scan is already running"
	case s.job != nil && s.job.Running:
		return "a cleanup is running"
	}
	return ""
}

// startScan validates q and starts a background scan. Returns an error
// (and starts nothing) for invalid input, a missing folder, or when busy.
func (s *Server) startScan(q scanRequest) error {
	opts, err := q.options()
	if err != nil {
		return err
	}
	// Check folders up front so a typo is reported immediately, not after
	// the job has "started".
	if _, err := scan.NormalizeRoots(opts.Roots); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if b := s.busyLocked(); b != "" {
		return errors.New(b)
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.scanning, s.scanCancel, s.scanErr = true, cancel, ""
	s.progress = scan.Progress{Phase: "walking"}
	s.lastScan = &q
	opts.Progress = func(p scan.Progress) {
		s.mu.Lock()
		s.progress = p
		s.mu.Unlock()
	}
	go func() {
		rep, err := scan.Run(ctx, opts)
		cancel()
		s.mu.Lock()
		defer s.mu.Unlock()
		s.scanning, s.scanCancel = false, nil
		switch {
		case errors.Is(err, context.Canceled):
			s.scanErr = "Scan cancelled. Nothing was changed."
		case err != nil:
			s.scanErr = "Scan failed: " + err.Error()
		default:
			// The new report replaces the old one; removal marks were
			// about the old report's entry IDs, so they're reset.
			s.rep, s.removed = rep, map[int]bool{}
			s.repVersion++
		}
	}()
	return nil
}

// handleScan starts a scan (POST /api/scan).
func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	var q scanRequest
	if err := decode(r, &q); err != nil {
		writeError(w, http.StatusBadRequest, "bad request: "+err.Error())
		return
	}
	if err := s.startScan(q); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"started": true})
}

// handleScanCancel stops a running scan (POST /api/scan/cancel). The
// current report, if any, is kept.
func (s *Server) handleScanCancel(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	if s.scanCancel != nil {
		s.scanCancel()
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// liveReport returns a copy of the report without copies already removed
// this session, dropping groups left with fewer than two copies. Caller
// must hold s.mu and have checked s.rep != nil.
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

// cleanJob is the state of one background cleanup, shown in the progress
// dialog. All fields are guarded by Server.mu.
type cleanJob struct {
	ID          int             `json:"id"`
	Running     bool            `json:"running"`
	Action      clean.Action    `json:"action"`
	Applied     bool            `json:"applied"` // false = dry run
	Total       int             `json:"total"`
	TotalBytes  int64           `json:"totalBytes"`
	Done        int             `json:"done"`
	DoneBytes   int64           `json:"doneBytes"`
	Current     string          `json:"current"` // path being worked on
	Step        string          `json:"step"`    // e.g. "verifying", "removing"
	Removed     int             `json:"removed"`
	WouldRemove int             `json:"wouldRemove"`
	Refused     int             `json:"refused"`
	Failed      int             `json:"failed"`
	BytesFreed  int64           `json:"bytesFreed"`
	WouldFree   int64           `json:"bytesWouldFree"`
	Outcomes    []clean.Outcome `json:"outcomes"`
	Stopped     bool            `json:"stopped"` // cancelled by the user
	Error       string          `json:"error,omitempty"`
	LogPath     string          `json:"logPath,omitempty"`
	StartedAt   time.Time       `json:"startedAt"`
	FinishedAt  time.Time       `json:"finishedAt,omitempty"`

	cancel context.CancelFunc
}

// recentOutcomes is how many outcomes /api/status includes while a job is
// running (the full list is available once it finishes).
const recentOutcomes = 8

// snapshot returns a copy safe to encode after the lock is released. With
// full=false and the job still running, only the latest outcomes are
// included, keeping the once-a-second status poll small.
func (j *cleanJob) snapshot(full bool) cleanJob {
	cp := *j
	cp.cancel = nil
	if !full && j.Running && len(j.Outcomes) > recentOutcomes {
		cp.Outcomes = j.Outcomes[len(j.Outcomes)-recentOutcomes:]
	}
	cp.Outcomes = append([]clean.Outcome(nil), cp.Outcomes...)
	return cp
}

// cleanRequest is the body of POST /api/clean.
type cleanRequest struct {
	Remove  []int  `json:"remove"`
	Action  string `json:"action"`
	Apply   bool   `json:"apply"`
	Confirm string `json:"confirm"` // must be "DELETE" for an applied permanent delete
}

// handleClean validates a cleanup request and starts it as a background
// job (POST /api/clean). Invalid requests (unknown IDs, every copy of a
// group selected, unconfirmed permanent delete) are rejected with 400 here,
// before anything starts.
func (s *Server) handleClean(w http.ResponseWriter, r *http.Request) {
	var req cleanRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad request: "+err.Error())
		return
	}
	confirmed := req.Confirm == "DELETE"
	opts := clean.Options{
		Action: clean.Action(req.Action), Apply: req.Apply, ConfirmPermanent: confirmed,
		LogPath: s.cfg.LogPath, TrashFunc: s.cfg.TrashFunc,
	}
	if req.Apply && opts.Action == clean.Delete && !confirmed {
		writeError(w, http.StatusBadRequest, "type DELETE to confirm permanent deletion")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rep == nil {
		writeError(w, http.StatusConflict, "no report loaded - run a scan first")
		return
	}
	if b := s.busyLocked(); b != "" {
		writeError(w, http.StatusConflict, "busy - "+b)
		return
	}
	live := s.liveReport()
	if err := clean.Check(live, req.Remove, opts); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Totals for the progress bar: the selected copies' sizes.
	var totalBytes int64
	for _, id := range req.Remove {
		if _, e := live.FindEntry(id); e != nil {
			totalBytes += e.Size
		}
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.jobCount++
	job := &cleanJob{ID: s.jobCount, Running: true, Action: opts.Action, Applied: req.Apply,
		Total: len(req.Remove), TotalBytes: totalBytes, StartedAt: time.Now(), Outcomes: []clean.Outcome{}, cancel: cancel}
	s.job = job

	// Callbacks run inside clean.Run on the job goroutine; they take the
	// lock briefly to update the shared job state.
	opts.Starting = func(e report.Entry, step string) {
		s.mu.Lock()
		job.Current, job.Step = e.Path, step
		s.mu.Unlock()
	}
	opts.Progress = func(o clean.Outcome) {
		s.mu.Lock()
		defer s.mu.Unlock()
		job.Outcomes = append(job.Outcomes, o)
		job.Done++
		job.DoneBytes += o.Size
		switch o.Status {
		case clean.Removed:
			job.Removed++
			job.BytesFreed += o.Size
		case clean.WouldRemove:
			job.WouldRemove++
			job.WouldFree += o.Size
		case clean.Refused:
			job.Refused++
		case clean.Failed:
			job.Failed++
		}
	}

	remove := append([]int(nil), req.Remove...)
	go func() {
		res, err := clean.Run(ctx, live, remove, opts)
		cancel()
		s.mu.Lock()
		defer s.mu.Unlock()
		job.Running, job.FinishedAt, job.Current, job.Step = false, time.Now(), "", ""
		if res != nil {
			job.LogPath = res.LogPath
		}
		switch {
		case errors.Is(err, context.Canceled):
			job.Stopped = true
		case err != nil:
			job.Error = err.Error()
		}
		if req.Apply && res != nil {
			s.markRemovedLocked(res)
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"started": true, "id": job.ID, "total": job.Total})
}

// handleCleanStatus returns the latest cleanup job in full (all outcomes).
func (s *Server) handleCleanStatus(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job == nil {
		writeJSON(w, http.StatusOK, map[string]any{"job": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": s.job.snapshot(true)})
}

// handleCleanCancel asks a running cleanup to stop before its next item.
func (s *Server) handleCleanCancel(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	if s.job != nil && s.job.Running && s.job.cancel != nil {
		s.job.cancel()
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// markRemovedLocked records which report entries are gone after a cleanup:
// the removed copies themselves, plus anything inside a removed folder.
// Caller must hold s.mu.
func (s *Server) markRemovedLocked(res *clean.Result) {
	var folders []string
	changed := false
	for _, o := range res.Outcomes {
		if o.Status == clean.Removed {
			s.removed[o.EntryID] = true
			changed = true
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
	if changed {
		s.repVersion++
	}
}
