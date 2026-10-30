//go:build darwin

// macOS Trash support. We ask Finder to delete the item (via the built-in
// `osascript` AppleScript runner) rather than moving it into ~/.Trash
// ourselves, because only Finder records where it came from - which is what
// makes "Put Back" work. The first time, macOS asks the user to allow
// Twinsweep (or the terminal running it) to control Finder.
package trash

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// script reads the path from its arguments (argv) rather than having it
// pasted into the script text. This matters for security: a file named
// `x" & do shell script "rm -rf ~` would otherwise be able to inject its own
// AppleScript. Passing it as data means it is only ever treated as a path.
var script = []string{
	"-e", "on run argv",
	"-e", "tell application \"Finder\" to delete (POSIX file (item 1 of argv) as alias)",
	"-e", "end run",
}

// MoveToTrash sends path (file or folder) to the macOS Trash via Finder.
// Returns an error (and changes nothing) if Finder refuses, e.g. because
// automation permission was denied or the volume has no Trash.
func MoveToTrash(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	args := append(append([]string{}, script...), "--", abs)
	// Absolute path to osascript so a different program named osascript
	// earlier in $PATH can't be run instead.
	out, err := exec.Command("/usr/bin/osascript", args...).CombinedOutput() // #nosec G204 -- fixed binary, path passed as data
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "-1743") || strings.Contains(strings.ToLower(msg), "not allowed") {
			return fmt.Errorf("macOS blocked access to Finder; allow it in System Settings > Privacy & Security > Automation, then retry (%s)", msg)
		}
		return fmt.Errorf("Finder could not move it to the Trash: %s", msg)
	}
	if _, err := os.Lstat(abs); err == nil {
		return errors.New("the item is still present after moving to the Trash")
	}
	return nil
}
