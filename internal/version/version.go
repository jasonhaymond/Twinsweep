// Package version holds Twinsweep's single app version number.
//
// Where it sits: everything that needs to display or record the version (the
// `twinsweep version` command, the JSON scan report, the action log, the web
// review page) reads it from here, so there is exactly one place to bump it.
// Bump it in the same commit that adds the matching CHANGELOG.md entry.
package version

// Version is the semantic version (MAJOR.MINOR.PATCH) of this build.
const Version = "0.1.0"
