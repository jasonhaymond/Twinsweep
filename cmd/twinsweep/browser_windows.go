//go:build windows

// Opening the review page in the default browser on Windows. Split from
// browser_unix.go because finding the System32 folder needs a Windows API.
package main

import (
	"os/exec"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// openBrowser opens url (always our own http://127.0.0.1 sign-in link) in
// the default browser via rundll32's URL handler.
//
// The System32 path comes from Windows itself (GetSystemDirectory), NOT from
// the SystemRoot environment variable: an environment variable can be changed
// by whatever launched us, which would make this run a different program.
func openBrowser(url string) error {
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		return err
	}
	rundll := filepath.Join(sys, "rundll32.exe")
	return exec.Command(rundll, "url.dll,FileProtocolHandler", url).Start() // #nosec G204 -- OS-provided System32 binary; url is our own loopback link
}
