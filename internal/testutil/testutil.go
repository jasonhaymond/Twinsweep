// Package testutil holds small helpers shared by Twinsweep's test files for
// building throwaway folder trees on the real filesystem. Only imported by
// *_test.go files, so none of it ends up in the twinsweep binary.
package testutil

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Write creates a file (and any missing parent folders) with the given
// content under root, and returns its absolute path. Fails the test on error.
func Write(t testing.TB, root, rel, content string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	// Owner-only permissions: test files never need to be readable by others.
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// SetTime sets a file's modification time, so tests can create copies with
// distinct ages for the newest/oldest keep rules.
func SetTime(t testing.TB, path string, ts time.Time) {
	t.Helper()
	if err := os.Chtimes(path, ts, ts); err != nil {
		t.Fatal(err)
	}
}

// Symlink creates a symbolic link, or skips the test if the OS doesn't allow
// it (Windows without Developer Mode or admin rights).
func Symlink(t testing.TB, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks not available here: %v", err)
	}
}

// Exists reports whether a path exists (without following a final symlink).
func Exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
