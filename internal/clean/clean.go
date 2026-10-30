// Package clean removes duplicate copies chosen from a scan report - safely.
//
// Where it sits: called by `twinsweep clean` (CLI) and by the review page's
// "Move to Recycle Bin" / "Permanently delete" buttons (internal/web). This
// is the ONLY package in Twinsweep that removes anything from disk.
//
// The report is treated as a hint, not as truth. Files can change between
// the scan and the cleanup (edited, replaced, renamed, swapped for a
// symlink), and a report file can be hand-edited. So for every group, right
// before acting, this package:
//
//  1. Refuses outright if the selection would remove every copy in a group.
//  2. Works out which copies will survive - excluding any "kept" copy that
//     sits inside a folder that is itself selected for removal.
//  3. Re-reads one surviving copy from disk and confirms it still matches
//     the report (still a real file/folder, same size, same content hash).
//  4. For each copy to remove: confirms it is still a real file/folder (not
//     now a symlink), is not the same physical item as the kept copy (a
//     hardlink, or the same path spelled differently), hasn't been modified
//     since the scan, and that its full content hash equals the kept copy's
//     hash computed in step 3.
//
// Only then is it moved to the Recycle Bin/Trash (default) or, if the caller
// explicitly confirmed it, permanently deleted. Anything that fails a check
// is "refused" and left untouched. Every action taken is appended to an
// action log (see log.go).
//
// Known limit: there is an unavoidable tiny window between the final check
// and the removal call itself in which another program could change the
// file. This is inherent to any file tool on any OS.
package clean

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"twinsweep/internal/pathutil"
	"twinsweep/internal/report"
	"twinsweep/internal/scan"
	"twinsweep/internal/trash"
)

// Action is what to do with each copy selected for removal.
type Action string

const (
	Trash  Action = "trash"  // move to Recycle Bin / Trash (recoverable; default)
	Delete Action = "delete" // permanent delete (NOT recoverable)
)

// ErrNeedsConfirmation is returned when permanent deletion is requested
// without the explicit confirmation flag.
var ErrNeedsConfirmation = errors.New("permanent delete requires explicit confirmation (--yes-permanently-delete on the command line, or typing DELETE on the review page)")

// Options controls a cleanup run.
type Options struct {
	Action Action
	// Apply=false is a dry run: every check runs, nothing is changed.
	Apply bool
	// ConfirmPermanent must be true for Apply with Action=Delete.
	ConfirmPermanent bool
	// LogPath overrides where the action log is written ("" = default).
	LogPath string
	// TrashFunc overrides how items are trashed (tests use this). nil means
	// the real OS Recycle Bin/Trash.
	TrashFunc func(path string) error
	// Progress, if set, is called once per copy as soon as its outcome is
	// known, so the CLI can stream results.
	Progress func(Outcome)
}

// Status is the result for one selected copy.
type Status string

const (
	WouldRemove Status = "would-remove" // dry run: passed every check
	Removed     Status = "removed"      // trashed or deleted
	Refused     Status = "refused"      // failed a safety check; untouched
	Failed      Status = "failed"       // passed checks but the OS removal call failed; untouched
	InFolder    Status = "in-folder"    // inside a folder that is also selected; handled with that folder
)

// Outcome describes what happened (or would happen) to one selected copy.
type Outcome struct {
	EntryID  int         `json:"entryId"`
	GroupID  int         `json:"groupId"`
	Kind     report.Kind `json:"kind"`
	Path     string      `json:"path"`
	Size     int64       `json:"size"`
	Status   Status      `json:"status"`
	Reason   string      `json:"reason,omitempty"`
	KeptPath string      `json:"keptPath,omitempty"`
}

// Result summarizes a whole run.
type Result struct {
	Action         Action    `json:"action"`
	Applied        bool      `json:"applied"`
	Outcomes       []Outcome `json:"outcomes"`
	Removed        int       `json:"removed"`
	WouldRemove    int       `json:"wouldRemove"`
	Refused        int       `json:"refused"`
	Failed         int       `json:"failed"`
	BytesFreed     int64     `json:"bytesFreed"`
	BytesWouldFree int64     `json:"bytesWouldFree"`
	LogPath        string    `json:"logPath,omitempty"`
}

// Run checks (and, if opts.Apply, removes) the copies whose entry IDs are
// listed in remove.
//
// Returns an error WITHOUT changing anything if the request itself is
// invalid: unknown action, permanent delete not confirmed, an ID that isn't
// in the report, or a group with every copy selected. Otherwise it returns a
// Result listing each copy's outcome; individual refusals are not errors.
// If ctx is cancelled partway, it stops before the next copy and returns
// the partial Result plus ctx's error.
func Run(ctx context.Context, rep *report.Report, remove []int, opts Options) (*Result, error) {
	// --- Validate the request as a whole before touching anything. ---
	if opts.Action != Trash && opts.Action != Delete {
		return nil, fmt.Errorf("unknown action %q (use trash or delete)", opts.Action)
	}
	if opts.Apply && opts.Action == Delete && !opts.ConfirmPermanent {
		return nil, ErrNeedsConfirmation
	}
	trashFn := opts.TrashFunc
	if trashFn == nil {
		trashFn = trash.MoveToTrash
	}

	// Map each requested ID to its entry; IDs (not paths) are the only way
	// to select something, so nothing outside the report can be targeted.
	selected := map[int]bool{}
	for _, id := range remove {
		if g, e := rep.FindEntry(id); g == nil || e == nil {
			return nil, fmt.Errorf("copy #%d is not in this report", id)
		}
		selected[id] = true
	}
	// Rule 1: at least one copy of every group must be left unselected.
	for _, g := range rep.Groups() {
		n := 0
		for _, e := range g.Entries {
			if selected[e.ID] {
				n++
			}
		}
		if n > 0 && n == len(g.Entries) {
			return nil, fmt.Errorf("group %d: every copy is selected for removal; at least one copy must be kept", g.ID)
		}
	}

	// Paths of every selected folder, used for rule 2 and to avoid
	// double-handling files inside them.
	var folderTargets []string
	for _, g := range rep.FolderGroups {
		for _, e := range g.Entries {
			if selected[e.ID] {
				folderTargets = append(folderTargets, e.Path)
			}
		}
	}

	res := &Result{Action: opts.Action, Applied: opts.Apply}
	var log *actionLog
	if opts.Apply {
		// No audit trail, no removals: if the log can't be opened, stop now.
		var err error
		if log, err = openLog(opts.LogPath); err != nil {
			return nil, fmt.Errorf("cannot open action log: %w", err)
		}
		defer log.Close()
		res.LogPath = log.path
	}

	r := &runner{ctx: ctx, rep: rep, opts: opts, trashFn: trashFn, log: log, res: res, folderTargets: folderTargets}
	// Folder groups come first in rep.Groups(), so a folder is removed (or
	// refused) before any file group that refers to files inside it.
	for _, g := range rep.Groups() {
		var targets, others []report.Entry
		for _, e := range g.Entries {
			if selected[e.ID] {
				targets = append(targets, e)
			} else {
				others = append(others, e)
			}
		}
		if len(targets) == 0 {
			continue
		}
		if err := r.group(g, targets, others); err != nil {
			return res, err
		}
	}
	return res, nil
}

// runner carries per-run state between groups.
type runner struct {
	ctx           context.Context
	rep           *report.Report
	opts          Options
	trashFn       func(string) error
	log           *actionLog
	res           *Result
	folderTargets []string
}

// record adds an outcome to the result, updates the totals, and notifies
// the progress callback.
func (r *runner) record(g *report.Group, e report.Entry, st Status, reason, kept string) {
	o := Outcome{EntryID: e.ID, GroupID: g.ID, Kind: g.Kind, Path: e.Path, Size: e.Size, Status: st, Reason: reason, KeptPath: kept}
	r.res.Outcomes = append(r.res.Outcomes, o)
	switch st {
	case WouldRemove:
		r.res.WouldRemove++
		r.res.BytesWouldFree += e.Size
	case Removed:
		r.res.Removed++
		r.res.BytesFreed += e.Size
	case Refused:
		r.res.Refused++
	case Failed:
		r.res.Failed++
	}
	if r.opts.Progress != nil {
		r.opts.Progress(o)
	}
}

// insideSelectedFolder returns the selected folder containing path, if any.
// A folder never counts as "inside" itself here.
func (r *runner) insideSelectedFolder(path string) (string, bool) {
	for _, f := range r.folderTargets {
		if !pathutil.Same(path, f) && pathutil.IsUnder(path, f) {
			return f, true
		}
	}
	return "", false
}

// group handles one duplicate group: steps 2-4 from the package comment.
func (r *runner) group(g *report.Group, targets, others []report.Entry) error {
	// Stop cleanly if cancelled, rather than recording every remaining copy
	// as "refused" because its verification read was interrupted.
	if err := r.ctx.Err(); err != nil {
		return err
	}
	// Files inside a folder that's also being removed go with that folder;
	// trying to remove them separately first would just break the folder's
	// verification. (If that folder is refused, they simply stay.)
	var real []report.Entry
	for _, t := range targets {
		if f, ok := r.insideSelectedFolder(t.Path); ok && g.Kind == report.KindFile {
			r.record(g, t, InFolder, "inside selected folder "+f, "")
			continue
		}
		real = append(real, t)
	}
	if len(real) == 0 {
		return nil
	}

	// Rule 2: survivors are unselected copies NOT inside a selected folder.
	var survivors []report.Entry
	for _, o := range others {
		if _, inside := r.insideSelectedFolder(o.Path); !inside {
			survivors = append(survivors, o)
		}
	}
	refuseAll := func(reason string) {
		for _, t := range real {
			r.record(g, t, Refused, reason, "")
		}
	}
	if len(survivors) == 0 {
		refuseAll("no copy would remain: every kept copy is inside a folder that is also selected for removal")
		return nil
	}

	// Rule 3: verify one surviving copy against the report.
	var ref string
	var kept report.Entry
	var keptInfo fs.FileInfo
	var lastErr error
	for _, s := range survivors {
		h, info, err := r.verifyKeeper(g, s)
		if err == nil {
			ref, kept, keptInfo = h, s, info
			break
		}
		lastErr = err
	}
	if keptInfo == nil {
		refuseAll(fmt.Sprintf("could not confirm the kept copy is still intact: %v", lastErr))
		return nil
	}

	// Rule 4: verify and act on each target.
	for _, t := range real {
		if err := r.ctx.Err(); err != nil {
			return err
		}
		if reason := r.verifyTarget(g, t, ref, keptInfo); reason != "" {
			r.record(g, t, Refused, reason, kept.Path)
			continue
		}
		if !r.opts.Apply {
			r.record(g, t, WouldRemove, "", kept.Path)
			continue
		}
		err := r.remove(g, t)
		r.log.write(r.opts.Action, g, t, kept.Path, ref, err)
		if err != nil {
			r.record(g, t, Failed, err.Error(), kept.Path)
			continue
		}
		r.record(g, t, Removed, "", kept.Path)
	}
	return nil
}

// isRealFile / isRealDir check the item's type exactly: a regular file or a
// plain folder, with no symlink/junction/device bits. On Windows this also
// rejects junctions and other reparse points.
func isRealFile(info fs.FileInfo) bool { return info.Mode().Type() == 0 }
func isRealDir(info fs.FileInfo) bool  { return info.Mode().Type() == fs.ModeDir }

// verifyKeeper re-reads a surviving copy and returns its current content
// hash (file) or signature (folder), after checking it still matches the
// report. Read-only.
func (r *runner) verifyKeeper(g *report.Group, e report.Entry) (string, fs.FileInfo, error) {
	info, err := os.Lstat(e.Path)
	if err != nil {
		return "", nil, fmt.Errorf("%s: %w", e.Path, err)
	}
	// Only compare against the report's hash if it was computed from real
	// contents (exact mode). Fast-mode folder "hashes" are name/size
	// signatures and fast-mode files have none.
	checkHash := r.rep.Mode == report.ModeExact && g.Hash != ""
	if g.Kind == report.KindFolder {
		if !isRealDir(info) {
			return "", nil, fmt.Errorf("%s is no longer a plain folder", e.Path)
		}
		sig, files, bytes, err := scan.FolderSignature(r.ctx, e.Path)
		if err != nil {
			return "", nil, err
		}
		if files != g.FileCount || bytes != g.Size || (checkHash && sig != g.Hash) {
			return "", nil, fmt.Errorf("%s has changed since the scan", e.Path)
		}
		return sig, info, nil
	}
	if !isRealFile(info) {
		return "", nil, fmt.Errorf("%s is no longer a regular file", e.Path)
	}
	if info.Size() != g.Size {
		return "", nil, fmt.Errorf("%s has changed size since the scan", e.Path)
	}
	h, _, err := scan.FullHash(r.ctx, e.Path)
	if err != nil {
		return "", nil, err
	}
	if checkHash && h != g.Hash {
		return "", nil, fmt.Errorf("%s contents have changed since the scan", e.Path)
	}
	return h, info, nil
}

// verifyTarget re-checks a copy selected for removal. Returns "" if it is
// safe to remove, otherwise a plain-language reason it was refused.
func (r *runner) verifyTarget(g *report.Group, t report.Entry, ref string, keptInfo fs.FileInfo) string {
	info, err := os.Lstat(t.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "no longer exists"
		}
		return err.Error()
	}
	// Two paths to one physical item (hardlink, or a different spelling of
	// the same path on a case-insensitive drive): "removing the duplicate"
	// would remove the kept copy too. Never.
	if os.SameFile(info, keptInfo) {
		return "is the same physical item as the kept copy"
	}
	if g.Kind == report.KindFolder {
		if !isRealDir(info) {
			return "is no longer a plain folder (it may have been replaced by a link)"
		}
		sig, _, _, err := scan.FolderSignature(r.ctx, t.Path)
		if err != nil {
			return err.Error()
		}
		if sig != ref {
			return "contents no longer match the kept copy"
		}
		return ""
	}
	if !isRealFile(info) {
		return "is no longer a regular file (it may have been replaced by a link)"
	}
	if info.Size() != t.Size || !info.ModTime().Equal(t.ModTime) {
		return "has been modified since the scan"
	}
	h, _, err := scan.FullHash(r.ctx, t.Path)
	if err != nil {
		return err.Error()
	}
	if h != ref {
		return "contents no longer match the kept copy"
	}
	return ""
}

// remove performs the actual removal of one verified copy.
func (r *runner) remove(g *report.Group, t report.Entry) error {
	if r.opts.Action == Trash {
		return r.trashFn(t.Path)
	}
	if g.Kind == report.KindFolder {
		// Safe to recurse: FolderSignature just confirmed the folder holds
		// only plain files and folders - no links that could lead outside it.
		return os.RemoveAll(t.Path)
	}
	return os.Remove(t.Path)
}
