// Tests for the path helpers. These guard the "is the kept copy inside a
// folder we are about to remove?" check in the cleanup engine, so a bug here
// is a data-loss bug, not a cosmetic one.
package pathutil

import (
	"path/filepath"
	"runtime"
	"testing"
)

// TestIsUnder proves the folder-containment check gets the tricky cases right:
// a sibling folder whose name merely starts the same ("Data2" vs "Data") is
// NOT inside, and a drive/filesystem root contains everything below it.
func TestIsUnder(t *testing.T) {
	root := string(filepath.Separator)
	if runtime.GOOS == "windows" {
		root = `C:\`
	}
	data := filepath.Join(root, "Data")
	cases := []struct {
		path, dir string
		want      bool
	}{
		{data, data, true}, // a folder counts as "under" itself
		{filepath.Join(data, "a", "b.txt"), data, true},                       // nested file
		{filepath.Join(root, "Data2", "b.txt"), data, false},                  // look-alike sibling prefix
		{filepath.Join(root, "Dat"), data, false},                             // shorter path
		{filepath.Join(data, "x"), root, true},                                // root contains everything
		{filepath.Join(data, "..", "Other"), data, false},                     // ".." is resolved before comparing
		{filepath.Join(data, "sub") + string(filepath.Separator), data, true}, // trailing slash ignored
	}
	for _, c := range cases {
		if got := IsUnder(c.path, c.dir); got != c.want {
			t.Errorf("IsUnder(%q, %q) = %v, want %v", c.path, c.dir, got, c.want)
		}
	}
}

// TestCaseFolding proves that on Windows/macOS two spellings of one folder
// are treated as the same place (so a tampered or hand-edited report cannot
// sneak the kept copy past the safety check by changing letter case), while
// on Linux they stay distinct because they really are different files there.
func TestCaseFolding(t *testing.T) {
	a := filepath.Join(string(filepath.Separator), "Photos", "IMG.jpg")
	b := filepath.Join(string(filepath.Separator), "photos", "img.JPG")
	want := runtime.GOOS == "windows" || runtime.GOOS == "darwin"
	if got := Same(a, b); got != want {
		t.Fatalf("Same(%q,%q) = %v, want %v on %s", a, b, got, want, runtime.GOOS)
	}
}
