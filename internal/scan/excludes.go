// Exclude rules: which files and folders the scanner skips entirely.
//
// Patterns are "globs" (shell-style wildcards): `*` matches any run of
// characters within ONE name, `?` matches one character, `[abc]` matches one
// of a set. Example: `*.tmp` matches "report.tmp"; `node_modules` matches any
// folder of that name anywhere in the tree.
//
// A pattern containing a path separator (/ or \) is matched against the whole
// absolute path instead, e.g. `C:\Users\*\AppData` or `/home/*/.cache`.
package scan

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

// DefaultExcludes are OS housekeeping folders that are never what a user
// means by "my files": recycle bins/trash cans (scanning them would offer to
// "clean up" things already deleted), search indexes, and version stores.
// Disabled with --no-default-excludes.
var DefaultExcludes = []string{
	"$RECYCLE.BIN",              // Windows Recycle Bin (per drive)
	"System Volume Information", // Windows restore points / indexing
	".Trashes",                  // macOS trash on external volumes
	".Trash",                    // macOS user trash / freedesktop shared trash
	".Trash-*",                  // freedesktop (Linux) per-user trash on a drive
	".Spotlight-V100",           // macOS search index
	".fseventsd",                // macOS file-change journal
	".DocumentRevisions-V100",   // macOS document versions
	".TemporaryItems",           // macOS temp folder on volumes
}

// matcher holds the compiled exclude list.
type matcher struct {
	patterns []string
	// fold = compare ignoring case. True on Windows/macOS where file names
	// are case-insensitive, so `*.JPG` and `*.jpg` behave the same.
	fold bool
}

// newMatcher validates and combines the user's patterns with the defaults.
// An invalid pattern (e.g. an unclosed "[") is an error up front rather than
// a pattern that silently never matches.
func newMatcher(user []string, useDefaults bool) (matcher, error) {
	m := matcher{fold: runtime.GOOS == "windows" || runtime.GOOS == "darwin"}
	var all []string
	if useDefaults {
		all = append(all, DefaultExcludes...)
	}
	for _, p := range user {
		// filepath.Match with an empty name only reports whether the
		// pattern itself is well-formed.
		if _, err := filepath.Match(p, ""); err != nil {
			return m, fmt.Errorf("invalid --exclude pattern %q: %w", p, err)
		}
		all = append(all, p)
	}
	for _, p := range all {
		if m.fold {
			p = strings.ToLower(p)
		}
		m.patterns = append(m.patterns, filepath.FromSlash(p))
	}
	return m, nil
}

// match reports whether a file/folder should be skipped. name is its base
// name ("photo.jpg"), full its absolute path.
func (m matcher) match(name, full string) bool {
	if m.fold {
		name, full = strings.ToLower(name), strings.ToLower(full)
	}
	for _, p := range m.patterns {
		target := name
		if strings.ContainsRune(p, filepath.Separator) {
			target = full
		}
		if ok, _ := filepath.Match(p, target); ok {
			return true
		}
	}
	return false
}
