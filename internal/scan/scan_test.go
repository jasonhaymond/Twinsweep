// Scanner tests. Each builds a real folder tree in a temporary directory
// (deleted automatically afterwards) and checks what the scanner reports.
// Several of these are safety tests: a scanner that reports a non-duplicate
// as a duplicate, or a file as a duplicate of itself, would let cleanup
// offer to remove someone's only copy.
package scan

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"twinsweep/internal/report"
	tu "twinsweep/internal/testutil"
)

// run scans with exact mode + folders by default; mod tweaks the options.
func run(t *testing.T, roots []string, mod func(*Options)) *report.Report {
	t.Helper()
	o := Options{Roots: roots, Mode: report.ModeExact, MinSize: 1, Folders: true, DefaultExcludes: true}
	if mod != nil {
		mod(&o)
	}
	rep, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if err := rep.Validate(); err != nil {
		t.Fatal(err)
	}
	return rep
}

// groupPaths returns each group's sorted entry paths, relative to root, for
// easy comparison.
func groupPaths(t *testing.T, root string, gs []report.Group) [][]string {
	var out [][]string
	for _, g := range gs {
		var ps []string
		for _, e := range g.Entries {
			rel, err := filepath.Rel(root, e.Path)
			if err != nil {
				t.Fatal(err)
			}
			ps = append(ps, filepath.ToSlash(rel))
		}
		sort.Strings(ps)
		out = append(out, ps)
	}
	sort.Slice(out, func(i, j int) bool { return strings.Join(out[i], ",") < strings.Join(out[j], ",") })
	return out
}

// equal compares two [][]string.
func equal(a, b [][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if strings.Join(a[i], "|") != strings.Join(b[i], "|") {
			return false
		}
	}
	return true
}

// TestExactDuplicates proves identical files are grouped, and a same-size
// file with different content is NOT (size alone is never enough).
func TestExactDuplicates(t *testing.T) {
	d := t.TempDir()
	tu.Write(t, d, "a.txt", "hello")
	tu.Write(t, d, "sub/b.txt", "hello")
	tu.Write(t, d, "c.txt", "world") // same size, different bytes
	rep := run(t, []string{d}, func(o *Options) { o.Folders = false })
	got := groupPaths(t, d, rep.FileGroups)
	if want := [][]string{{"a.txt", "sub/b.txt"}}; !equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if rep.FileGroups[0].Hash == "" {
		t.Fatal("exact-mode group has no hash")
	}
}

// TestLargeFilesDifferingInMiddle proves the partial (head+tail) hash is
// only a filter: two large files with identical starts and ends but a
// different middle byte are not reported as duplicates.
func TestLargeFilesDifferingInMiddle(t *testing.T) {
	d := t.TempDir()
	body := strings.Repeat("x", 300_000)
	mid := body[:150_000] + "Y" + body[150_001:]
	tu.Write(t, d, "one.bin", body)
	tu.Write(t, d, "two.bin", mid)
	tu.Write(t, d, "three.bin", body)
	rep := run(t, []string{d}, func(o *Options) { o.Folders = false })
	got := groupPaths(t, d, rep.FileGroups)
	if want := [][]string{{"one.bin", "three.bin"}}; !equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestHardlinksAreNotDuplicates proves two names for the same physical file
// are not reported: removing one would free no space, and the user's backup
// tool may depend on the link.
func TestHardlinksAreNotDuplicates(t *testing.T) {
	d := t.TempDir()
	a := tu.Write(t, d, "a.txt", "linked content")
	if err := os.Link(a, filepath.Join(d, "b.txt")); err != nil {
		t.Skipf("hardlinks not supported: %v", err)
	}
	rep := run(t, []string{d}, func(o *Options) { o.Folders = false })
	if len(rep.FileGroups) != 0 {
		t.Fatalf("hardlinks reported as duplicates: %v", groupPaths(t, d, rep.FileGroups))
	}
	if rep.Stats.HardlinksCollapsed != 1 {
		t.Fatalf("HardlinksCollapsed = %d, want 1", rep.Stats.HardlinksCollapsed)
	}
}

// TestSymlinksNeverFollowed proves a symlink to a file is not treated as a
// second copy (removing "the duplicate" link target would remove the only
// real file) and makes its folder ineligible as a duplicate folder.
func TestSymlinksNeverFollowed(t *testing.T) {
	d := t.TempDir()
	tu.Write(t, d, "A/real.txt", "data")
	tu.Write(t, d, "B/real.txt", "data")
	tu.Symlink(t, filepath.Join(d, "A", "real.txt"), filepath.Join(d, "B", "link.txt"))
	tu.Write(t, d, "A/link.txt", "data") // so A and B have the same names
	rep := run(t, []string{d}, nil)
	for _, g := range rep.FileGroups {
		for _, e := range g.Entries {
			if strings.HasSuffix(e.Path, filepath.Join("B", "link.txt")) {
				t.Fatal("symlink reported as a duplicate copy")
			}
		}
	}
	if len(rep.FolderGroups) != 0 {
		t.Fatal("folder containing a symlink was reported as a duplicate folder")
	}
	if rep.Stats.Skipped != 1 {
		t.Fatalf("Skipped = %d, want 1", rep.Stats.Skipped)
	}
}

// TestEmptyFilesIgnoredByDefault proves zero-byte files (which are all
// "identical") are skipped unless --min-size 0 is given.
func TestEmptyFilesIgnoredByDefault(t *testing.T) {
	d := t.TempDir()
	tu.Write(t, d, "e1", "")
	tu.Write(t, d, "e2", "")
	if rep := run(t, []string{d}, func(o *Options) { o.Folders = false }); len(rep.FileGroups) != 0 {
		t.Fatal("empty files grouped with MinSize=1")
	}
	if rep := run(t, []string{d}, func(o *Options) { o.Folders = false; o.MinSize = 0 }); len(rep.FileGroups) != 1 {
		t.Fatal("empty files not grouped with MinSize=0")
	}
}

// TestExcludes proves excluded folders are not scanned at all, and that
// default excludes skip OS trash folders (so already-deleted files are never
// offered for "cleanup").
func TestExcludes(t *testing.T) {
	d := t.TempDir()
	tu.Write(t, d, "keep/a.txt", "same")
	tu.Write(t, d, "node_modules/a.txt", "same")
	tu.Write(t, d, "$RECYCLE.BIN/a.txt", "same")
	rep := run(t, []string{d}, func(o *Options) { o.Excludes = []string{"node_modules"} })
	if len(rep.FileGroups) != 0 || len(rep.FolderGroups) != 0 {
		t.Fatalf("excluded content was scanned: %v", groupPaths(t, d, rep.FileGroups))
	}
	if rep.Stats.Excluded != 2 {
		t.Fatalf("Excluded = %d, want 2", rep.Stats.Excluded)
	}
	if _, err := Run(context.Background(), Options{Roots: []string{d}, Excludes: []string{"[unclosed"}}); err == nil {
		t.Fatal("invalid exclude pattern accepted")
	}
}

// TestNestedRootsDoNotSelfDuplicate proves that passing a folder and one of
// its subfolders doesn't make every file look like a duplicate of itself -
// which would let cleanup "remove the extra copy" that is really the only one.
func TestNestedRootsDoNotSelfDuplicate(t *testing.T) {
	d := t.TempDir()
	tu.Write(t, d, "sub/only.txt", "unique")
	rep := run(t, []string{d, filepath.Join(d, "sub"), d}, nil)
	if len(rep.FileGroups) != 0 || len(rep.FolderGroups) != 0 {
		t.Fatal("a file was reported as a duplicate of itself")
	}
	if len(rep.Roots) != 1 {
		t.Fatalf("roots = %v, want only the outer folder", rep.Roots)
	}
}

// TestDuplicateFolders proves an identical folder tree is reported once as a
// folder group, its matching subfolders and files are not repeated, and the
// standalone FolderSignature (used by cleanup) agrees with the scanner.
func TestDuplicateFolders(t *testing.T) {
	d := t.TempDir()
	for _, top := range []string{"A", "B"} {
		tu.Write(t, d, top+"/x.txt", "x content")
		tu.Write(t, d, top+"/s/y.txt", "y content")
	}
	tu.Write(t, d, "C/other.txt", "different")
	rep := run(t, []string{d}, nil)
	if got, want := groupPaths(t, d, rep.FolderGroups), [][]string{{"A", "B"}}; !equal(got, want) {
		t.Fatalf("folder groups %v, want %v", got, want)
	}
	if len(rep.FileGroups) != 0 {
		t.Fatalf("file groups inside duplicate folders were repeated: %v", groupPaths(t, d, rep.FileGroups))
	}
	g := rep.FolderGroups[0]
	if g.FileCount != 2 || g.Size != int64(len("x content")+len("y content")) {
		t.Fatalf("folder totals wrong: %d files, %d bytes", g.FileCount, g.Size)
	}
	sig, _, _, err := FolderSignature(context.Background(), filepath.Join(d, "A"))
	if err != nil || sig != g.Hash {
		t.Fatalf("FolderSignature = %q (%v), scanner hash %q", sig, err, g.Hash)
	}
}

// TestFoldersWithOneDifferentFile proves one changed byte anywhere in a tree
// means the folders are NOT duplicates, while the files that do match are
// still reported individually.
func TestFoldersWithOneDifferentFile(t *testing.T) {
	d := t.TempDir()
	tu.Write(t, d, "A/x.txt", "x content")
	tu.Write(t, d, "A/s/y.txt", "y content")
	tu.Write(t, d, "B/x.txt", "x content")
	tu.Write(t, d, "B/s/y.txt", "y cOntent")
	rep := run(t, []string{d}, nil)
	if len(rep.FolderGroups) != 0 {
		t.Fatalf("non-identical folders grouped: %v", groupPaths(t, d, rep.FolderGroups))
	}
	if got, want := groupPaths(t, d, rep.FileGroups), [][]string{{"A/x.txt", "B/x.txt"}}; !equal(got, want) {
		t.Fatalf("file groups %v, want %v", got, want)
	}
}

// TestSiblingDuplicatesUnderDuplicateParents proves a duplicate pair sitting
// side by side (P/x and P/x2) is still reported when P itself matches Q.
func TestSiblingDuplicatesUnderDuplicateParents(t *testing.T) {
	d := t.TempDir()
	for _, top := range []string{"P", "Q"} {
		tu.Write(t, d, top+"/x/f.txt", "same")
		tu.Write(t, d, top+"/x2/f.txt", "same")
	}
	rep := run(t, []string{d}, nil)
	got := groupPaths(t, d, rep.FolderGroups)
	want := [][]string{{"P", "Q"}, {"P/x", "P/x2", "Q/x", "Q/x2"}}
	if !equal(got, want) {
		t.Fatalf("folder groups %v, want %v", got, want)
	}
}

// TestFastMode proves fast mode matches on name+size without reading
// contents - including a false match that exact mode correctly rejects.
// (Cleanup re-verifies contents, so a fast-mode false match can't cause a
// wrong removal; see the clean package tests.)
func TestFastMode(t *testing.T) {
	d := t.TempDir()
	tu.Write(t, d, "one/Report.PDF", "aaaa")
	tu.Write(t, d, "two/report.pdf", "bbbb")
	rep := run(t, []string{d}, func(o *Options) { o.Mode = report.ModeFast; o.Folders = false })
	if len(rep.FileGroups) != 1 || rep.FileGroups[0].Hash != "" || rep.Stats.FilesHashed != 0 {
		t.Fatalf("fast mode: %d groups, hash %q, %d hashed", len(rep.FileGroups), rep.FileGroups[0].Hash, rep.Stats.FilesHashed)
	}
	if exact := run(t, []string{d}, func(o *Options) { o.Folders = false }); len(exact.FileGroups) != 0 {
		t.Fatal("exact mode grouped files with different contents")
	}
}

// TestCancel proves a cancelled scan stops and returns an error instead of
// a partial (misleading) report.
func TestCancel(t *testing.T) {
	d := t.TempDir()
	tu.Write(t, d, "a", "x")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, Options{Roots: []string{d}}); err == nil {
		t.Fatal("cancelled scan returned no error")
	}
}
