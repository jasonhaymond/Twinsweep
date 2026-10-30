//go:build !unix && !windows

// Fallback for platforms with no known way to identify a physical file
// (e.g. Plan 9, WebAssembly). Every path is treated as a distinct file, so
// hardlinks there would be reported as duplicates - harmless, just not
// space-saving. None of the release builds target these platforms.
package scan

// fileKey always reports "unknown identity" on this platform.
func fileKey(path string) (string, bool) { return "", false }
