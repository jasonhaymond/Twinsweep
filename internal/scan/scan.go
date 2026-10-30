// Package scan walks folders and finds duplicate files and duplicate folders.
//
// Where it sits: called by `twinsweep scan` and by `twinsweep serve` (which
// scans in the background while the review page shows progress). It only
// ever READS the disk; it never modifies, moves, or deletes anything. Its
// output is a report.Report.
//
// How exact-mode detection works (each stage only looks at files that
// survived the previous one, so most files are never read at all):
//
//  1. Walk every folder and record each file's size.
//  2. Group files by size. A file with a unique size can't have a duplicate.
//  3. Drop hardlinks (two names for the same physical data - see fileid_*.go).
//  4. Hash the first + last 64 KiB of each remaining file; regroup.
//  5. Hash the whole file (SHA-256) for files still in a group; regroup.
//     Files with equal full hashes are reported as duplicates.
//
// Fast mode skips 4-5 and groups on (file name, size) only.
// Folder detection is in folders.go.
//
// Safety: symbolic links, Windows junctions, cloud placeholders and other
// special entries are never followed or reported; the folder that contains
// one is marked "incomplete" and is never offered as a duplicate folder,
// because we could not compare everything inside it.
package scan

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"twinsweep/internal/pathutil"
	"twinsweep/internal/report"
	"twinsweep/internal/version"
)

// Options controls one scan. Zero values get sensible defaults in Run.
type Options struct {
	Roots           []string    // folders to scan, in priority order (used by the "first-root" keep rule)
	Mode            report.Mode // exact (default) or fast
	MinSize         int64       // ignore files smaller than this many bytes (1 = skip empty files)
	Excludes        []string    // extra glob patterns to skip (see excludes.go)
	DefaultExcludes bool        // also skip OS trash/index folders
	Folders         bool        // also detect duplicate folders
	Workers         int         // parallel file readers (default 4)
	// Progress, if set, is called a few times per second with counters so a
	// terminal or web page can show what's happening. Called from a
	// background goroutine (Go's lightweight thread).
	Progress func(Progress)
}

// Progress is a snapshot of scan activity.
type Progress struct {
	Phase     string `json:"phase"` // "walking", "hashing", "folders", "done"
	FilesSeen int64  `json:"filesSeen"`
	BytesSeen int64  `json:"bytesSeen"`
	HashDone  int64  `json:"hashDone"`
	HashTotal int64  `json:"hashTotal"`
}

// fileRec is the scanner's internal record of one regular file.
type fileRec struct {
	path, name string
	size       int64
	mod        time.Time
	root       int    // index into the roots list
	dir        int    // index into scanner.dirs of the containing folder
	partial    string // head+tail hash (stage 4), "" if not computed
	hash       string // full SHA-256 (stage 5), "" if not computed
	failed     bool   // could not be read; excluded from all groups
}

// dirRec is the scanner's internal record of one folder.
type dirRec struct {
	path, name string
	parent     int // index of parent folder, -1 for a scan root
	root       int
	mod        time.Time
	files      []int // indexes into scanner.files
	dirs       []int // indexes into scanner.dirs
	// incomplete = something inside could not be compared (a symlink, an
	// excluded or unreadable item). Such folders are never folder duplicates.
	incomplete bool

	// Filled in by the folder stage (folders.go).
	totalFiles int
	totalBytes int64
	shape      string // signature over names+sizes only
	content    string // signature over names+sizes+content hashes
}

// scanner holds all state for one Run.
type scanner struct {
	opts  Options
	excl  matcher
	files []fileRec
	dirs  []dirRec
	stats report.Stats

	mu    sync.Mutex // guards stats.Errors/ErrorCount and phase (written from worker goroutines)
	phase string

	// Atomic counters: safe to update from many goroutines at once and read
	// from the progress reporter without locking.
	filesSeen, bytesSeen   atomic.Int64
	hashDone, hashTotal    atomic.Int64
	filesHashed, bytesRead atomic.Int64
}

// Run performs a complete scan and returns the report.
//
// Inputs: opts (see Options). ctx lets the caller cancel; on cancellation Run
// returns ctx's error and no report. Side effects: reads files only.
func Run(ctx context.Context, opts Options) (*report.Report, error) {
	start := time.Now()
	// Fill in defaults and reject bad option values early.
	if opts.Mode == "" {
		opts.Mode = report.ModeExact
	}
	if opts.Mode != report.ModeExact && opts.Mode != report.ModeFast {
		return nil, fmt.Errorf("unknown mode %q (use exact or fast)", opts.Mode)
	}
	if opts.Workers <= 0 {
		opts.Workers = 4
	}
	if opts.MinSize < 0 {
		return nil, errors.New("minimum size cannot be negative")
	}
	roots, err := NormalizeRoots(opts.Roots)
	if err != nil {
		return nil, err
	}
	excl, err := newMatcher(opts.Excludes, opts.DefaultExcludes)
	if err != nil {
		return nil, err
	}
	s := &scanner{opts: opts, excl: excl}
	s.setPhase("walking")

	// Background progress reporter: wakes every 250 ms and hands a snapshot
	// to the caller until the scan finishes.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	if opts.Progress != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.NewTicker(250 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					opts.Progress(s.snapshot())
				}
			}
		}()
	}
	defer func() {
		close(stop)
		wg.Wait()
		if opts.Progress != nil {
			s.setPhase("done")
			opts.Progress(s.snapshot())
		}
	}()

	// Stage 1: walk each root.
	for i, root := range roots {
		if err := s.walk(ctx, i, root); err != nil {
			return nil, err
		}
	}
	s.propagateIncomplete()

	// Stages 2-5: duplicate files.
	var fileGroups [][]int
	if opts.Mode == report.ModeFast {
		fileGroups = s.fastGroups()
	} else {
		fileGroups, err = s.exactGroups(ctx)
		if err != nil {
			return nil, err
		}
	}

	// Duplicate folders, then hide file groups fully explained by them.
	var folderGroups [][]int
	if opts.Folders {
		folderGroups, err = s.folderGroups(ctx)
		if err != nil {
			return nil, err
		}
		fileGroups = s.suppressCovered(fileGroups, folderGroups)
	}

	rep := s.buildReport(roots, fileGroups, folderGroups)
	rep.Stats.DurationMs = time.Since(start).Milliseconds()
	return rep, nil
}

// NormalizeRoots turns the user's folder arguments into clean absolute paths
// and drops any folder that is inside another one in the list.
//
// Why drop nested roots: scanning both "D:\Photos" and "D:\Photos\2024"
// would visit every file in 2024 twice, and each file would then look like a
// duplicate of itself - and cleanup could remove "one of the two copies",
// which is actually the only copy. The first-listed of two identical roots
// is kept so root order (used by the first-root rule) is preserved.
//
// Each root must exist and be a real folder (not a symlink to one; links are
// never followed anywhere in Twinsweep).
func NormalizeRoots(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, errors.New("no folders to scan")
	}
	abs := make([]string, 0, len(in))
	for _, p := range in {
		a, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		info, err := os.Lstat(a)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%s: folder not found", a)
		}
		if err != nil {
			return nil, err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return nil, fmt.Errorf("%s is a symbolic link; pass the real folder path instead", a)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("%s is not a folder", a)
		}
		abs = append(abs, a)
	}
	var out []string
	for i, a := range abs {
		drop := false
		for j, b := range abs {
			if i == j {
				continue
			}
			// Same folder twice: keep only the first occurrence.
			if pathutil.Same(a, b) {
				if j < i {
					drop = true
				}
				continue
			}
			// a is inside b: b's scan already covers a.
			if pathutil.IsUnder(a, b) {
				drop = true
			}
		}
		if !drop {
			out = append(out, a)
		}
	}
	return out, nil
}

// setPhase records the current stage name for progress reports.
func (s *scanner) setPhase(p string) {
	s.mu.Lock()
	s.phase = p
	s.mu.Unlock()
}

// snapshot copies the live counters into a Progress value.
func (s *scanner) snapshot() Progress {
	s.mu.Lock()
	phase := s.phase
	s.mu.Unlock()
	return Progress{
		Phase: phase, FilesSeen: s.filesSeen.Load(), BytesSeen: s.bytesSeen.Load(),
		HashDone: s.hashDone.Load(), HashTotal: s.hashTotal.Load(),
	}
}

// recordError counts a problem (unreadable folder, permission denied...) and
// keeps the first MaxStoredErrors messages for the report. Safe to call from
// worker goroutines.
func (s *scanner) recordError(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.ErrorCount++
	if len(s.stats.Errors) < report.MaxStoredErrors {
		s.stats.Errors = append(s.stats.Errors, fmt.Sprintf(format, args...))
	}
}

// walk records every file and folder under one root.
//
// filepath.WalkDir visits a folder before its contents ("pre-order"), so a
// child's parent folder is always already in dirIndex when the child is seen.
// It never follows symbolic links.
func (s *scanner) walk(ctx context.Context, rootIdx int, root string) error {
	dirIndex := map[string]int{} // folder path -> index in s.dirs
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr // stop the walk entirely on cancellation
		}
		parent := -1
		if path != root {
			parent = dirIndex[filepath.Dir(path)]
		}
		markParent := func() {
			if parent >= 0 {
				s.dirs[parent].incomplete = true
			}
		}

		// WalkDir reports problems here: e.g. permission denied reading a
		// folder. That folder (already recorded on the first visit) and its
		// parent can no longer be fully compared.
		if err != nil {
			s.recordError("%s: %v", path, err)
			if idx, ok := dirIndex[path]; ok {
				s.dirs[idx].incomplete = true
			}
			markParent()
			return nil // keep walking everything else
		}

		name := d.Name()
		if path != root && s.excl.match(name, path) {
			s.stats.Excluded++
			markParent()
			if d.IsDir() {
				return fs.SkipDir // don't descend into an excluded folder
			}
			return nil
		}

		t := d.Type()
		switch {
		case t.IsDir():
			var mod time.Time
			if info, ierr := d.Info(); ierr == nil {
				mod = info.ModTime()
			}
			idx := len(s.dirs)
			s.dirs = append(s.dirs, dirRec{path: path, name: name, parent: parent, root: rootIdx, mod: mod})
			dirIndex[path] = idx
			if parent >= 0 {
				s.dirs[parent].dirs = append(s.dirs[parent].dirs, idx)
			}
			s.stats.DirsSeen++

		case t.IsRegular():
			info, ierr := d.Info()
			if ierr != nil {
				s.recordError("%s: %v", path, ierr)
				markParent()
				return nil
			}
			idx := len(s.files)
			s.files = append(s.files, fileRec{
				path: path, name: name, size: info.Size(), mod: info.ModTime(),
				root: rootIdx, dir: parent,
			})
			s.dirs[parent].files = append(s.dirs[parent].files, idx)
			s.stats.FilesSeen++
			s.stats.BytesSeen += info.Size()
			s.filesSeen.Add(1)
			s.bytesSeen.Add(info.Size())

		default:
			// Symlink, junction, cloud placeholder, device, socket, pipe...
			// Never followed, never reported.
			s.stats.Skipped++
			markParent()
		}
		return nil
	})
}

// propagateIncomplete marks every ancestor of an incomplete folder as
// incomplete too: if "A/B" contains a symlink, "A" can't be a verified
// duplicate either. Children always have higher indexes than parents (see
// walk), so one backwards pass reaches every ancestor.
func (s *scanner) propagateIncomplete() {
	for i := len(s.dirs) - 1; i >= 0; i-- {
		if s.dirs[i].incomplete && s.dirs[i].parent >= 0 {
			s.dirs[s.dirs[i].parent].incomplete = true
		}
	}
}

// collapseHardlinks removes all but one path for each physical file in a
// candidate list (see fileid_unix.go for why). Returns the reduced list.
func (s *scanner) collapseHardlinks(idxs []int) []int {
	seen := map[string]bool{}
	out := idxs[:0:0]
	for _, i := range idxs {
		if key, ok := fileKey(s.files[i].path); ok {
			if seen[key] {
				s.stats.HardlinksCollapsed++
				continue
			}
			seen[key] = true
		}
		out = append(out, i)
	}
	return out
}

// bucketsOf groups file indexes by a key and returns only groups of 2+.
// Results are sorted so output order is stable between runs.
func bucketsOf(idxs []int, key func(i int) (string, bool)) [][]int {
	m := map[string][]int{}
	for _, i := range idxs {
		if k, ok := key(i); ok {
			m[k] = append(m[k], i)
		}
	}
	var out [][]int
	for _, b := range m {
		if len(b) >= 2 {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a][0] < out[b][0] })
	return out
}

// sizeCandidates is stage 2+3: every file at least MinSize bytes that shares
// its size with another file, with hardlinks collapsed.
func (s *scanner) sizeCandidates() [][]int {
	all := make([]int, 0, len(s.files))
	for i := range s.files {
		if s.files[i].size < s.opts.MinSize {
			s.stats.TooSmall++
			continue
		}
		all = append(all, i)
	}
	var out [][]int
	for _, b := range bucketsOf(all, func(i int) (string, bool) {
		return strconv.FormatInt(s.files[i].size, 10), true
	}) {
		if b = s.collapseHardlinks(b); len(b) >= 2 {
			out = append(out, b)
		}
	}
	return out
}

// fastGroups groups files by (name ignoring case, size) without reading
// them. Ignoring case means "IMG_001.JPG" and "img_001.jpg" match, which is
// what copying between Windows, macOS and Linux tends to produce.
func (s *scanner) fastGroups() [][]int {
	all := make([]int, 0, len(s.files))
	for i := range s.files {
		if s.files[i].size < s.opts.MinSize {
			s.stats.TooSmall++
			continue
		}
		all = append(all, i)
	}
	var out [][]int
	for _, b := range bucketsOf(all, func(i int) (string, bool) {
		return strings.ToLower(s.files[i].name) + "\x00" + strconv.FormatInt(s.files[i].size, 10), true
	}) {
		if b = s.collapseHardlinks(b); len(b) >= 2 {
			out = append(out, b)
		}
	}
	return out
}

// exactGroups runs stages 2-5 and returns groups of byte-identical files.
func (s *scanner) exactGroups(ctx context.Context) ([][]int, error) {
	buckets := s.sizeCandidates()
	s.setPhase("hashing")

	// Stage 4: partial hash of every candidate.
	var all []int
	for _, b := range buckets {
		all = append(all, b...)
	}
	err := s.parallel(ctx, all, func(f *fileRec) (int64, error) {
		h, whole, n, err := partialHash(ctx, f.path, f.size)
		if err != nil {
			return n, err
		}
		f.partial = h
		if whole {
			f.hash = h // small file: partial hash already covered every byte
		}
		s.filesHashed.Add(1)
		return n, nil
	})
	if err != nil {
		return nil, err
	}

	// Regroup on (size, partial hash), skipping unreadable files.
	var stage2 [][]int
	for _, b := range buckets {
		stage2 = append(stage2, bucketsOf(b, func(i int) (string, bool) {
			f := &s.files[i]
			return f.partial, !f.failed
		})...)
	}

	// Stage 5: full hash where the partial hash didn't already cover it.
	var need []int
	for _, b := range stage2 {
		for _, i := range b {
			if s.files[i].hash == "" && !s.files[i].failed {
				need = append(need, i)
			}
		}
	}
	if err := s.parallel(ctx, need, s.fullHashWork(ctx)); err != nil {
		return nil, err
	}

	var groups [][]int
	for _, b := range stage2 {
		groups = append(groups, bucketsOf(b, func(i int) (string, bool) {
			f := &s.files[i]
			return f.hash, !f.failed && f.hash != ""
		})...)
	}
	return groups, nil
}

// fullHashWork returns the work function that computes a file's full hash;
// shared by stage 5 and the folder stage.
func (s *scanner) fullHashWork(ctx context.Context) func(f *fileRec) (int64, error) {
	return func(f *fileRec) (int64, error) {
		h, n, err := FullHash(ctx, f.path)
		if err != nil {
			return n, err
		}
		f.hash = h
		return n, nil
	}
}

// parallel runs work on each listed file using opts.Workers goroutines.
//
// Each worker only writes to its own fileRec, so no locking is needed for
// the records themselves. A file that fails (permission denied, deleted
// mid-scan, I/O error) is marked failed, recorded as an error, and simply
// drops out of every group - one bad file never aborts the whole scan.
// Returns ctx.Err() if the scan was cancelled.
func (s *scanner) parallel(ctx context.Context, idxs []int, work func(f *fileRec) (int64, error)) error {
	s.hashTotal.Add(int64(len(idxs)))
	jobs := make(chan int) // a Go channel: a queue workers pull file indexes from
	var wg sync.WaitGroup
	for w := 0; w < s.opts.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				f := &s.files[i]
				n, err := work(f)
				s.bytesRead.Add(n)
				s.hashDone.Add(1)
				if err != nil && ctx.Err() == nil {
					f.failed = true
					s.recordError("%s: %v", f.path, err)
				}
			}
		}()
	}
feed:
	for _, i := range idxs {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break feed // stop handing out work; workers finish their current file
		}
	}
	close(jobs)
	wg.Wait()
	return ctx.Err()
}

// suppressCovered drops file groups whose every copy lives inside a folder
// that is already reported as a duplicate folder - otherwise one copied
// "Photos" folder would show up as one folder group plus thousands of
// redundant file groups. A file group with even one copy outside those
// folders is kept in full.
func (s *scanner) suppressCovered(fileGroups, folderGroups [][]int) [][]int {
	if len(folderGroups) == 0 {
		return fileGroups
	}
	covered := make([]bool, len(s.dirs))
	for _, g := range folderGroups {
		for _, d := range g {
			covered[d] = true
		}
	}
	isCovered := func(fi int) bool {
		for d := s.files[fi].dir; d >= 0; d = s.dirs[d].parent {
			if covered[d] {
				return true
			}
		}
		return false
	}
	var out [][]int
	for _, g := range fileGroups {
		for _, fi := range g {
			if !isCovered(fi) {
				out = append(out, g)
				break
			}
		}
	}
	return out
}

// buildReport converts internal index groups into the public report format,
// largest reclaimable space first, and numbers every group and entry.
func (s *scanner) buildReport(roots []string, fileIdx, folderIdx [][]int) *report.Report {
	rep := &report.Report{
		Schema: report.SchemaVersion, Tool: report.ToolName, Version: version.Version,
		CreatedAt: time.Now().UTC(), Mode: s.opts.Mode, Roots: roots,
		MinSize: s.opts.MinSize, Excludes: s.opts.Excludes, Folders: s.opts.Folders,
		FolderGroups: []report.Group{}, FileGroups: []report.Group{},
	}
	for _, g := range folderIdx {
		first := s.dirs[g[0]]
		grp := report.Group{Kind: report.KindFolder, Size: first.totalBytes, FileCount: first.totalFiles, Hash: first.content}
		if s.opts.Mode == report.ModeFast {
			grp.Hash = first.shape
		}
		for _, d := range g {
			dr := s.dirs[d]
			grp.Entries = append(grp.Entries, report.Entry{Path: dr.path, Size: dr.totalBytes, ModTime: dr.mod, Root: dr.root, FileCount: dr.totalFiles})
		}
		rep.FolderGroups = append(rep.FolderGroups, grp)
	}
	for _, g := range fileIdx {
		first := s.files[g[0]]
		grp := report.Group{Kind: report.KindFile, Size: first.size, Hash: first.hash}
		for _, fi := range g {
			f := s.files[fi]
			grp.Entries = append(grp.Entries, report.Entry{Path: f.path, Size: f.size, ModTime: f.mod, Root: f.root})
		}
		rep.FileGroups = append(rep.FileGroups, grp)
	}

	// Biggest wins first; ties broken by path so output order is stable.
	order := func(gs []report.Group) {
		for i := range gs {
			sort.Slice(gs[i].Entries, func(a, b int) bool { return gs[i].Entries[a].Path < gs[i].Entries[b].Path })
		}
		sort.SliceStable(gs, func(a, b int) bool {
			ra, rb := gs[a].Reclaimable(), gs[b].Reclaimable()
			if ra != rb {
				return ra > rb
			}
			return gs[a].Entries[0].Path < gs[b].Entries[0].Path
		})
	}
	order(rep.FolderGroups)
	order(rep.FileGroups)

	// Number groups 1..n and entries 1..m across the whole report.
	gid, eid := 0, 0
	for _, g := range rep.Groups() {
		gid++
		g.ID = gid
		for i := range g.Entries {
			eid++
			g.Entries[i].ID = eid
		}
	}

	s.mu.Lock()
	rep.Stats = s.stats
	s.mu.Unlock()
	rep.Stats.FilesHashed = s.filesHashed.Load()
	rep.Stats.BytesHashed = s.bytesRead.Load()
	return rep
}
