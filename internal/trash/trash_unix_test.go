//go:build unix && !darwin

// Tests for the freedesktop (Linux/BSD) trash. They point $XDG_DATA_HOME at a
// temporary folder so the real user's Trash is never touched, then check the
// result matches the spec closely enough for file managers to restore it.
package trash

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTrashFileAndFolder proves a file and a folder are moved into
// Trash/files with a matching .trashinfo recording the original path, and
// that a second item with the same name doesn't overwrite the first.
func TestTrashFileAndFolder(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	work := t.TempDir()

	file := filepath.Join(work, "My Report.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	folder := filepath.Join(work, "album")
	if err := os.MkdirAll(filepath.Join(folder, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{file, folder} {
		if err := MoveToTrash(p); err != nil {
			t.Fatalf("MoveToTrash(%s): %v", p, err)
		}
		if _, err := os.Lstat(p); err == nil {
			t.Fatalf("%s still exists", p)
		}
	}
	trash := filepath.Join(data, "Trash")
	info, err := os.ReadFile(filepath.Join(trash, "info", "My Report.txt.trashinfo"))
	if err != nil {
		t.Fatal(err)
	}
	// The path must be URL-encoded per the spec ("My Report" -> "My%20Report").
	if !strings.Contains(string(info), "Path="+strings.ReplaceAll(file, " ", "%20")) || !strings.Contains(string(info), "DeletionDate=") {
		t.Fatalf("bad trashinfo:\n%s", info)
	}
	if _, err := os.Stat(filepath.Join(trash, "files", "album", "inner")); err != nil {
		t.Fatalf("folder not in trash: %v", err)
	}

	// Same name again: must become "My Report.txt.2", not overwrite.
	if err := os.WriteFile(file, []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := MoveToTrash(file); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(trash, "files", "My Report.txt.2")); err != nil || string(b) != "y" {
		t.Fatalf("collision handling failed: %q %v", b, err)
	}
}

// TestRefusesSymlinkedPrivateTrash proves a pre-created .Trash-$UID that is
// really a symlink (another user trying to capture our deleted files) is
// rejected rather than used.
func TestRefusesSymlinkedPrivateTrash(t *testing.T) {
	d := t.TempDir()
	link := filepath.Join(d, ".Trash-test")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Skip(err)
	}
	if err := ensurePrivateTopTrash(link); err == nil {
		t.Fatal("symlinked .Trash-$UID was accepted")
	}
}
