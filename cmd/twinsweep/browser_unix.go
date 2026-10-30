//go:build !windows

// Opening the review page in the default browser on macOS, Linux and BSD.
// Windows is in browser_windows.go.
package main

import (
	"os/exec"
	"runtime"
)

// openBrowser opens url (always our own http://127.0.0.1 sign-in link) in
// the default browser. macOS's launcher has a fixed location, so it's called
// by absolute path (a look-alike "open" earlier in PATH can't be run
// instead). xdg-open's location varies by Linux distribution, so it's found
// via PATH.
func openBrowser(url string) error {
	if runtime.GOOS == "darwin" {
		return exec.Command("/usr/bin/open", url).Start() // #nosec G204 -- fixed system binary; url is our own loopback link
	}
	return exec.Command("xdg-open", url).Start() // #nosec G204 -- standard desktop launcher; url is our own loopback link
}
