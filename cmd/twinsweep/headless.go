// Deciding whether Twinsweep is running somewhere a browser can be shown.
//
// Why: running `twinsweep` with no command (e.g. double-clicking the .exe)
// opens the browser app. That only makes sense when there is a desktop
// session to show a browser in. Over SSH, or on a Linux server with no
// graphical display, there is no browser to open (and 127.0.0.1 on the
// server isn't reachable from your own browser without a tunnel), so
// Twinsweep prints its command-line help instead.
package main

import "runtime"

// isHeadless reports whether there's no usable desktop session, using the
// real environment and OS.
func isHeadless(getenv func(string) string) bool {
	return headlessFor(runtime.GOOS, getenv)
}

// headlessFor is the decision itself, separated from the real OS/environment
// so tests can check every combination.
//
//   - Any OS: an SSH session (SSH_CONNECTION / SSH_CLIENT / SSH_TTY set)
//     counts as headless - the desktop, if any, belongs to someone else's
//     screen, and the browser can't reach the server's 127.0.0.1 directly.
//   - Windows and macOS otherwise always have a desktop session when a
//     person runs a program, so they're not headless.
//   - Linux/BSD: a graphical session sets DISPLAY (X11) or WAYLAND_DISPLAY;
//     without either there's nowhere to show a browser.
func headlessFor(goos string, getenv func(string) string) bool {
	for _, v := range []string{"SSH_CONNECTION", "SSH_CLIENT", "SSH_TTY"} {
		if getenv(v) != "" {
			return true
		}
	}
	switch goos {
	case "windows", "darwin":
		return false
	default:
		return getenv("DISPLAY") == "" && getenv("WAYLAND_DISPLAY") == ""
	}
}
