// Cleanup safety tests. These are the most important tests in Twinsweep:
// each one proves a specific way of losing data is blocked. They scan a real
// temporary folder tree, then run cleanup against it. "Trash" is redirected
// to a test folder (via Options.TrashFunc) so the tests never touch the real
// Recycle Bin; the real trash implementations are tested in internal/trash.
package clean

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"twinsweep/internal/report"
	"twinsweep/internal/scan"
	tu "twinsweep/internal/testutil"
)

// fixture is a scanned temp tree plus a fake trash.
type fixture struct {
	t       *testing.T
	dir     string
	rep     *report.Report
	trashed []string
	logPath string
}

// newFixture writes the given files (relative path -> content), scans them,
// and returns the fixture.
func newFixture(t *testing.T, files map[string]string, folders bool) *fixture {
	t.Helper()
	d := t.TempDir()
	for rel, content := range files {
		tu.Write(t, d, rel, content)
	}
	f := &fixture{t: t, dir: d, logPath: filepath.Join(t.TempDir(), "actions.log")}
	f.rescan(folders)
	return f
}

// rescan re-runs the scanner over the fixture folder.
func (f *fixture) rescan(folders bool) {
	f.t.Helper()
	rep, err := scan.Run(context.Background(), scan.Options{Roots: []string{f.dir}, MinSize: 1, Folders: folders})
	if err != nil {
		f.t.Fatal(err)
	}
	f.rep = rep
}

// path returns the absolute path of a fixture-relative path.
func (f *fixture) path(rel string) string { return filepath.Join(f.dir, filepath.FromSlash(rel)) }

// id finds the report entry ID for a fixture-relative path.
func (f *fixture) id(rel string) int {
	f.t.Helper()
	for _, g := range f.rep.Groups() {
		for _, e := range g.Entries {
			if e.Path == f.path(rel) {
				return e.ID
			}
		}
	}
	f.t.Fatalf("%s not in report", rel)
	return 0
}

// opts returns options that "trash" by moving into a test folder.
func (f *fixture) opts(apply bool) Options {
	trashDir := filepath.Join(f.t.TempDir(), "trash")
	return Options{
		Action: Trash, Apply: apply, LogPath: f.logPath,
		TrashFunc: func(p string) error {
			f.trashed = append(f.trashed, p)
			if err := os.MkdirAll(trashDir, 0o755); err != nil {
				return err
			}
			return os.Rename(p, filepath.Join(trashDir, filepath.Base(p)))
		},
	}
}

// clean runs cleanup on the given relative paths.
func (f *fixture) clean(o Options, rels ...string) (*Result, error) {
	var ids []int
	for _, r := range rels {
		ids = append(ids, f.id(r))
	}
	return Run(context.Background(), f.rep, ids, o)
}

// status returns the outcome status for a relative path.
func status(res *Result, f *fixture, rel string) (Status, string) {
	for _, o := range res.Outcomes {
		if o.Path == f.path(rel) {
			return o.Status, o.Reason
		}
	}
	return "", ""
}

var pair = map[string]string{"keep/a.txt": "same bytes", "dupe/a.txt": "same bytes"}

// TestDryRunChangesNothing proves the default (no --apply) only reports.
func TestDryRunChangesNothing(t *testing.T) {
	f := newFixture(t, pair, false)
	res, err := f.clean(f.opts(false), "dupe/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := status(res, f, "dupe/a.txt"); st != WouldRemove {
		t.Fatalf("status %q, want would-remove", st)
	}
	if !tu.Exists(f.path("dupe/a.txt")) || len(f.trashed) != 0 || tu.Exists(f.logPath) {
		t.Fatal("dry run modified the disk")
	}
}

// TestTrashRemovesOnlySelected proves an applied run trashes exactly the
// selected copy, keeps the other, and writes the action log.
func TestTrashRemovesOnlySelected(t *testing.T) {
	f := newFixture(t, pair, false)
	res, err := f.clean(f.opts(true), "dupe/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 1 || tu.Exists(f.path("dupe/a.txt")) || !tu.Exists(f.path("keep/a.txt")) {
		t.Fatalf("removed=%d; dupe exists=%v keep exists=%v", res.Removed, tu.Exists(f.path("dupe/a.txt")), tu.Exists(f.path("keep/a.txt")))
	}
	log, err := os.ReadFile(f.logPath)
	if err != nil || !strings.Contains(string(log), `"result":"ok"`) || !strings.Contains(string(log), "dupe") {
		t.Fatalf("action log missing entry: %s (%v)", log, err)
	}
}

// TestRefusesSelectingEveryCopy proves the "always keep one" rule: a request
// to remove all copies of a group is rejected before anything is touched.
func TestRefusesSelectingEveryCopy(t *testing.T) {
	f := newFixture(t, pair, false)
	if _, err := f.clean(f.opts(true), "dupe/a.txt", "keep/a.txt"); err == nil {
		t.Fatal("removing every copy was allowed")
	}
	if !tu.Exists(f.path("dupe/a.txt")) || !tu.Exists(f.path("keep/a.txt")) {
		t.Fatal("files touched despite rejected request")
	}
}

// TestRefusesUnknownID proves only things the scanner found can be targeted:
// an ID not in the report (e.g. a forged web request) is rejected outright.
func TestRefusesUnknownID(t *testing.T) {
	f := newFixture(t, pair, false)
	if _, err := Run(context.Background(), f.rep, []int{9999}, f.opts(true)); err == nil {
		t.Fatal("unknown entry ID accepted")
	}
}

// TestPermanentDeleteNeedsConfirmation proves permanent deletion can't
// happen without the explicit confirmation flag, and works with it.
func TestPermanentDeleteNeedsConfirmation(t *testing.T) {
	f := newFixture(t, pair, false)
	o := f.opts(true)
	o.Action = Delete
	if _, err := f.clean(o, "dupe/a.txt"); !errors.Is(err, ErrNeedsConfirmation) {
		t.Fatalf("err = %v, want ErrNeedsConfirmation", err)
	}
	if !tu.Exists(f.path("dupe/a.txt")) {
		t.Fatal("deleted without confirmation")
	}
	o.ConfirmPermanent = true
	if res, err := f.clean(o, "dupe/a.txt"); err != nil || res.Removed != 1 || tu.Exists(f.path("dupe/a.txt")) || len(f.trashed) != 0 {
		t.Fatalf("confirmed delete failed: %v", err)
	}
}

// TestRefusesTargetModifiedAfterScan proves a "duplicate" the user edited
// after scanning (so it's no longer a duplicate) is left alone.
func TestRefusesTargetModifiedAfterScan(t *testing.T) {
	f := newFixture(t, pair, false)
	tu.Write(t, f.dir, "dupe/a.txt", "SAME bytes") // same size, new content and mtime
	res, err := f.clean(f.opts(true), "dupe/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := status(res, f, "dupe/a.txt"); st != Refused || !tu.Exists(f.path("dupe/a.txt")) {
		t.Fatalf("modified file was not refused (status %q)", st)
	}
}

// TestRefusesContentSwapWithSameTimestamp proves the content hash is checked
// even when size and modification time were made to look unchanged.
func TestRefusesContentSwapWithSameTimestamp(t *testing.T) {
	f := newFixture(t, pair, false)
	p := f.path("dupe/a.txt")
	info, _ := os.Stat(p)
	tu.Write(t, f.dir, "dupe/a.txt", "SAME bytes")
	tu.SetTime(t, p, info.ModTime())
	res, _ := f.clean(f.opts(true), "dupe/a.txt")
	if st, _ := status(res, f, "dupe/a.txt"); st != Refused || !tu.Exists(p) {
		t.Fatalf("content-swapped file not refused (status %q)", st)
	}
}

// TestRefusesWhenKeeperMissing proves nothing is removed if the copy we'd
// be keeping has disappeared since the scan (it may have been the last one).
func TestRefusesWhenKeeperMissing(t *testing.T) {
	f := newFixture(t, pair, false)
	if err := os.Remove(f.path("keep/a.txt")); err != nil {
		t.Fatal(err)
	}
	res, _ := f.clean(f.opts(true), "dupe/a.txt")
	if st, _ := status(res, f, "dupe/a.txt"); st != Refused || !tu.Exists(f.path("dupe/a.txt")) {
		t.Fatalf("removed a copy while the keeper is gone (status %q)", st)
	}
}

// TestRefusesWhenKeeperChanged proves nothing is removed if the kept copy
// no longer matches - the "duplicate" is then the only copy of the old data.
func TestRefusesWhenKeeperChanged(t *testing.T) {
	f := newFixture(t, pair, false)
	tu.Write(t, f.dir, "keep/a.txt", "SAME bytes")
	res, _ := f.clean(f.opts(true), "dupe/a.txt")
	if st, _ := status(res, f, "dupe/a.txt"); st != Refused || !tu.Exists(f.path("dupe/a.txt")) {
		t.Fatalf("removed a copy while the keeper changed (status %q)", st)
	}
}

// TestRefusesSymlinkSwap proves that if a target is replaced by a symlink
// after the scan, it is refused (removing through a link could remove the
// link's real target - possibly the keeper).
func TestRefusesSymlinkSwap(t *testing.T) {
	f := newFixture(t, pair, false)
	p := f.path("dupe/a.txt")
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	tu.Symlink(t, f.path("keep/a.txt"), p)
	res, _ := f.clean(f.opts(true), "dupe/a.txt")
	if st, _ := status(res, f, "dupe/a.txt"); st != Refused || !tu.Exists(f.path("keep/a.txt")) {
		t.Fatalf("symlink swap not refused (status %q)", st)
	}
}

// TestRefusesHardlinkToKeeper proves a tampered/stale report that lists two
// names of one physical file as "duplicates" can't trick cleanup into
// removing the only data.
func TestRefusesHardlinkToKeeper(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "only copy"}, false)
	a := f.path("a.txt")
	b := f.path("b.txt")
	if err := os.Link(a, b); err != nil {
		t.Skipf("hardlinks unsupported: %v", err)
	}
	h, _, _ := scan.FullHash(context.Background(), a)
	info, _ := os.Stat(a)
	f.rep.FileGroups = []report.Group{{ID: 1, Kind: report.KindFile, Hash: h, Size: info.Size(), Entries: []report.Entry{
		{ID: 1, Path: a, Size: info.Size(), ModTime: info.ModTime()},
		{ID: 2, Path: b, Size: info.Size(), ModTime: info.ModTime()},
	}}}
	res, _ := Run(context.Background(), f.rep, []int{2}, f.opts(true))
	if res.Outcomes[0].Status != Refused || !tu.Exists(b) {
		t.Fatalf("hardlink to keeper not refused (status %q)", res.Outcomes[0].Status)
	}
}

// TestTamperedReportCannotRemoveUniqueFile proves a hand-edited report that
// claims two different files are duplicates can't get either removed.
func TestTamperedReportCannotRemoveUniqueFile(t *testing.T) {
	f := newFixture(t, map[string]string{"x.txt": "unique one", "y.txt": "unique two"}, false)
	var es []report.Entry
	for i, rel := range []string{"x.txt", "y.txt"} {
		info, _ := os.Stat(f.path(rel))
		es = append(es, report.Entry{ID: i + 1, Path: f.path(rel), Size: info.Size(), ModTime: info.ModTime()})
	}
	f.rep.FileGroups = []report.Group{{ID: 1, Kind: report.KindFile, Size: es[0].Size, Entries: es}} // no hash, like fast mode
	res, _ := Run(context.Background(), f.rep, []int{2}, f.opts(true))
	if res.Outcomes[0].Status != Refused || !tu.Exists(f.path("y.txt")) {
		t.Fatalf("unique file removed via tampered report (status %q)", res.Outcomes[0].Status)
	}
}

// folderTree is two identical folders plus a third copy of one file outside
// them, so both a folder group and a file group exist.
var folderTree = map[string]string{
	"A/f.txt": "folder file", "A/s/g.txt": "nested",
	"B/f.txt": "folder file", "B/s/g.txt": "nested",
	"C/f2.txt": "folder file",
}

// TestFolderTrashAndFilesInside proves a whole duplicate folder is trashed
// as one item, and files inside it that were also selected are handled with
// it rather than separately.
func TestFolderTrashAndFilesInside(t *testing.T) {
	f := newFixture(t, folderTree, true)
	res, err := f.clean(f.opts(true), "B", "B/f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := status(res, f, "B"); st != Removed || tu.Exists(f.path("B")) || !tu.Exists(f.path("A/s/g.txt")) {
		t.Fatalf("folder B status %q", st)
	}
	if st, _ := status(res, f, "B/f.txt"); st != InFolder {
		t.Fatalf("file inside removed folder: status %q, want in-folder", st)
	}
}

// TestRefusesWhenKeeperIsInsideRemovedFolder proves the trickiest case: the
// copy "kept" in a file group lives inside a folder being removed in the
// same run. Removing the other copies would then leave none at all.
func TestRefusesWhenKeeperIsInsideRemovedFolder(t *testing.T) {
	f := newFixture(t, folderTree, true)
	// Folder group: remove B (keep A). File group {A/f, B/f, C/f2}: keep
	// B/f (inside B!) and remove A/f and C/f2.
	res, err := f.clean(f.opts(true), "B", "A/f.txt", "C/f2.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"A/f.txt", "C/f2.txt"} {
		if st, _ := status(res, f, rel); st != Refused || !tu.Exists(f.path(rel)) {
			t.Fatalf("%s: status %q, want refused and still present", rel, st)
		}
	}
}

// TestFastModeReportIsVerified proves a fast-mode (name+size only) false
// match is caught by cleanup's content check and not removed.
func TestFastModeReportIsVerified(t *testing.T) {
	d := t.TempDir()
	tu.Write(t, d, "1/r.pdf", "aaaa")
	tu.Write(t, d, "2/r.pdf", "bbbb")
	rep, err := scan.Run(context.Background(), scan.Options{Roots: []string{d}, Mode: report.ModeFast, MinSize: 1})
	if err != nil || len(rep.FileGroups) != 1 {
		t.Fatalf("setup: %v, %d groups", err, len(rep.FileGroups))
	}
	f := &fixture{t: t, dir: d, rep: rep, logPath: filepath.Join(t.TempDir(), "log")}
	res, _ := f.clean(f.opts(true), "2/r.pdf")
	if st, _ := status(res, f, "2/r.pdf"); st != Refused || !tu.Exists(f.path("2/r.pdf")) {
		t.Fatalf("fast-mode false match removed (status %q)", st)
	}
}

// TestCancelStopsBeforeNextItem proves a cancelled cleanup stops instead of
// carrying on removing things.
func TestCancelStopsBeforeNextItem(t *testing.T) {
	f := newFixture(t, pair, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, f.rep, []int{f.id("dupe/a.txt")}, f.opts(true))
	if err == nil || !tu.Exists(f.path("dupe/a.txt")) {
		t.Fatal("cancelled cleanup still removed files")
	}
}
