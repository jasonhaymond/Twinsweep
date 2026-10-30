//go:build windows

// Tests for the Windows Recycle Bin implementation.
package trash

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestRefusesNetworkPath proves that a path on a network share - where
// Windows would silently delete permanently instead of recycling - is
// refused with ErrNoTrash before the shell is ever called.
func TestRefusesNetworkPath(t *testing.T) {
	err := MoveToTrash(`\\127.0.0.1\twinsweep-test-share-that-does-not-exist\file.txt`)
	if !errors.Is(err, ErrNoTrash) {
		t.Fatalf("err = %v, want ErrNoTrash", err)
	}
}

// TestRealRecycleBin sends a small temp file to the real Recycle Bin. It is
// skipped unless TWINSWEEP_TEST_RECYCLE=1, because it leaves an item in the
// user's Recycle Bin ("twinsweep-recycle-test.txt", safe to empty).
func TestRealRecycleBin(t *testing.T) {
	if os.Getenv("TWINSWEEP_TEST_RECYCLE") != "1" {
		t.Skip("set TWINSWEEP_TEST_RECYCLE=1 to test the real Recycle Bin")
	}
	p := filepath.Join(t.TempDir(), "twinsweep-recycle-test.txt")
	if err := os.WriteFile(p, []byte("delete me"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := MoveToTrash(p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(p); err == nil {
		t.Fatal("file still exists")
	}
}
