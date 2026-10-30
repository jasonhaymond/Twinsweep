//go:build !unix && !windows

// Fallback for platforms with no known trash (not shipped in release
// builds). Always refuses, so "move to trash" can never turn into a
// permanent delete by accident.
package trash

// MoveToTrash always returns ErrNoTrash on this platform.
func MoveToTrash(path string) error { return ErrNoTrash }
