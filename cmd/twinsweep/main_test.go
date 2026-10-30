// Tests for the command-line entry point: what happens with no command
// (browser app vs. help), and that unknown commands still fail cleanly.
package main

import (
	"bytes"
	"strings"
	"testing"
)

// env builds a getenv function from a map.
func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// TestHeadlessDetection proves the "is there a desktop?" decision for each
// OS and session type, so a bare `twinsweep` never tries to open a browser
// over SSH or on a display-less server.
func TestHeadlessDetection(t *testing.T) {
	cases := []struct {
		goos string
		vars map[string]string
		want bool
	}{
		{"windows", nil, false},
		{"windows", map[string]string{"SSH_CONNECTION": "10.0.0.5 51000 10.0.0.9 22"}, true},
		{"darwin", nil, false},
		{"darwin", map[string]string{"SSH_TTY": "/dev/ttys001"}, true},
		{"linux", nil, true},
		{"linux", map[string]string{"DISPLAY": ":0"}, false},
		{"linux", map[string]string{"WAYLAND_DISPLAY": "wayland-0"}, false},
		{"linux", map[string]string{"DISPLAY": ":0", "SSH_CLIENT": "10.0.0.5 51000 22"}, true},
		{"freebsd", nil, true},
	}
	for _, c := range cases {
		if got := headlessFor(c.goos, env(c.vars)); got != c.want {
			t.Errorf("%s %v: headless=%v, want %v", c.goos, c.vars, got, c.want)
		}
	}
}

// TestNoArgsHeadlessPrintsHelp proves that on a headless system a bare
// `twinsweep` prints usage (exit 2) instead of starting a server.
func TestNoArgsHeadlessPrintsHelp(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runWith(nil, &out, &errOut, env(map[string]string{"SSH_CONNECTION": "x"}))
	if code != exitUsage || !strings.Contains(errOut.String(), "No desktop session") || !strings.Contains(errOut.String(), "twinsweep scan") {
		t.Fatalf("code %d, stderr:\n%s", code, errOut.String())
	}
}

// TestUnknownCommand proves a typo is still an error, not silently treated
// as "open the browser app".
func TestUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runWith([]string{"scna"}, &out, &errOut, env(nil)); code != exitUsage {
		t.Fatalf("code %d, want %d", code, exitUsage)
	}
}
