//go:build unix

// Physical-file identity on Linux, macOS and the BSDs.
//
// The `//go:build unix` line above is a build constraint: Go only compiles
// this file when targeting a Unix-like OS. Windows uses fileid_windows.go.
//
// Why: a "hardlink" is a second name for the same data on disk. Two hardlinked
// paths look like duplicates (same size, same bytes) but removing one frees no
// space, and to the user they may be intentional (e.g. backup tools like
// rsnapshot rely on them). So the scanner collapses paths that are the same
// physical file into one before reporting duplicates.
package scan

import (
	"fmt"
	"os"
	"syscall"
)

// fileKey returns a string that is identical for two paths if and only if
// they are the same physical file: the device (filesystem) number plus the
// inode number (the filesystem's internal ID for the file's data).
// ok=false means the identity could not be determined; the caller then
// treats the path as a distinct file, which is the safe default.
func fileKey(path string) (key string, ok bool) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", false
	}
	st, isStat := info.Sys().(*syscall.Stat_t)
	if !isStat {
		return "", false
	}
	return fmt.Sprintf("%d:%d", st.Dev, st.Ino), true
}
