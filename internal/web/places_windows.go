//go:build windows

// Starting points for the folder picker on Windows: every drive letter that
// exists, plus the user's home folder.
package web

import (
	"os"

	"golang.org/x/sys/windows"
)

// places lists drive roots (C:\, D:\, ...) and the home folder.
// GetLogicalDrives returns a bitmask (bit 0 = A:, bit 1 = B:, ...) without
// touching the drives themselves, so an empty DVD drive or a sleeping
// network drive can't stall the picker.
func places() []place {
	var out []place
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, place{Name: "Home (" + home + ")", Path: home})
	}
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return out
	}
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) != 0 {
			root := string(rune('A'+i)) + `:\`
			out = append(out, place{Name: root, Path: root})
		}
	}
	return out
}
