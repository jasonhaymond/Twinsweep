//go:build !windows

// Starting points for the folder picker on macOS, Linux and BSD: the home
// folder, the filesystem root, and the usual places external drives appear.
package web

import "os"

// places lists home, "/", and any of /Volumes (macOS), /media and /mnt
// (Linux) that exist.
func places() []place {
	var out []place
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, place{Name: "Home (" + home + ")", Path: home})
	}
	out = append(out, place{Name: "/ (whole system)", Path: "/"})
	for _, p := range []string{"/Volumes", "/media", "/mnt"} {
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			out = append(out, place{Name: p, Path: p})
		}
	}
	return out
}
