// Package pathutil contains small path-comparison helpers shared by the
// scanner, the cleanup engine, and the keep rules.
//
// Why it exists: deciding "is this path inside that folder?" sounds trivial
// but has real safety consequences here. The cleanup engine uses it to make
// sure it never removes a folder that contains the copy we promised to keep.
// Getting it wrong (for example, treating "C:\Data2" as inside "C:\Data", or
// missing that "c:\data" and "C:\Data" are the same folder on Windows) could
// delete the last copy of someone's files. So the logic lives in one place,
// with tests.
package pathutil

import (
	"path/filepath"
	"runtime"
	"strings"
)

// caseInsensitive reports whether the current OS normally treats file names
// as case-insensitive. Windows (NTFS) and macOS (APFS default) do; Linux and
// the BSDs do not. When unsure we lean towards "insensitive" on those two
// platforms because that is the *safer* answer for a deletion tool: it makes
// us treat more paths as "the same" and therefore refuse more removals.
func caseInsensitive() bool {
	return runtime.GOOS == "windows" || runtime.GOOS == "darwin"
}

// normalize cleans a path (removes "..", duplicate slashes, trailing slash)
// and lower-cases it on case-insensitive platforms so two spellings of the
// same location compare equal.
func normalize(p string) string {
	p = filepath.Clean(p)
	if caseInsensitive() {
		p = strings.ToLower(p)
	}
	return p
}

// Same reports whether a and b name the same location as text, after
// cleaning and (on Windows/macOS) ignoring letter case. It does not touch the
// disk; use os.SameFile when you need to know whether two paths are the same
// physical file (e.g. hardlinks).
func Same(a, b string) bool {
	return normalize(a) == normalize(b)
}

// IsUnder reports whether path is dir itself or anywhere inside dir.
//
// Inputs: two absolute paths. Output: true/false. No disk access.
//
// The separator check matters: "C:\Data2\x" must NOT count as inside
// "C:\Data", so we require the character right after the folder prefix to be
// a path separator. A folder that already ends in a separator (a drive root
// like "C:\" or the Unix root "/") is handled by not adding a second one.
func IsUnder(path, dir string) bool {
	p, d := normalize(path), normalize(dir)
	if p == d {
		return true
	}
	if !strings.HasSuffix(d, string(filepath.Separator)) {
		d += string(filepath.Separator)
	}
	return strings.HasPrefix(p, d)
}
