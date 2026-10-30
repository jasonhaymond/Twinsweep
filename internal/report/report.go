// Package report defines the scan result ("report") that every other part of
// Twinsweep passes around, and reads/writes it as a JSON file.
//
// Where it sits: `twinsweep scan` produces a Report and can save it with
// --out; `twinsweep clean` and `twinsweep serve` load one back. The report is
// a *description* of what the scanner saw at scan time - the cleanup engine
// never trusts it blindly. Before removing anything, cleanup re-reads the real
// files and re-checks their contents (see internal/clean). That means a stale
// or hand-edited report can at worst cause a *genuine* duplicate to be
// removed; it can never cause a unique file to be removed.
//
// Security note: a report contains full file paths, which can reveal private
// information (user names, project names, document titles). Reports are
// therefore written with owner-only permissions (0600 on Unix).
package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SchemaVersion is bumped whenever the JSON layout changes in a way older
// versions of Twinsweep could not read correctly.
const SchemaVersion = 1

// ToolName is written into every report so `clean` can refuse to load an
// unrelated JSON file passed to --report by mistake.
const ToolName = "twinsweep"

// Mode says how duplicates were detected.
type Mode string

const (
	// ModeExact compares real file contents (SHA-256 hashes). Reliable.
	ModeExact Mode = "exact"
	// ModeFast only compares file name + size, without reading contents.
	// Quick but can be wrong; cleanup always re-verifies contents anyway.
	ModeFast Mode = "fast"
)

// Kind distinguishes a group of duplicate files from a group of duplicate
// folders.
type Kind string

const (
	KindFile   Kind = "file"
	KindFolder Kind = "folder"
)

// Entry is one copy inside a duplicate group: one file, or one folder.
type Entry struct {
	// ID is unique across the whole report. The web UI and cleanup refer to
	// copies by this number rather than by path, so a request can only ever
	// target something the scanner actually found.
	ID int `json:"id"`
	// Path is the absolute path of this copy.
	Path string `json:"path"`
	// Size is the file size in bytes, or for a folder the total size of all
	// files inside it.
	Size int64 `json:"size"`
	// ModTime is the last-modified time seen at scan time. Cleanup compares
	// it again so a file edited after the scan is not removed.
	ModTime time.Time `json:"modTime"`
	// Root is the index (into Report.Roots) of the scan folder this copy was
	// found under. Used by the "first-root" keep rule.
	Root int `json:"root"`
	// FileCount is how many files a folder entry contains (0 for files).
	FileCount int `json:"fileCount,omitempty"`
}

// Group is a set of two or more copies that are duplicates of each other.
type Group struct {
	ID   int  `json:"id"`
	Kind Kind `json:"kind"`
	// Hash is the SHA-256 content hash (files) or folder signature (folders)
	// shared by every entry. Empty for file groups found in fast mode, where
	// contents were never read.
	Hash string `json:"hash,omitempty"`
	// Size is the size of ONE copy, in bytes.
	Size int64 `json:"size"`
	// FileCount is the number of files inside each copy (folder groups only).
	FileCount int     `json:"fileCount,omitempty"`
	Entries   []Entry `json:"entries"`
}

// Reclaimable is how many bytes would be freed by keeping one copy and
// removing all the others.
func (g *Group) Reclaimable() int64 {
	if len(g.Entries) < 2 {
		return 0
	}
	return g.Size * int64(len(g.Entries)-1)
}

// Stats are counters gathered while scanning, shown in the summary so the
// user can tell what was (and was not) looked at.
type Stats struct {
	FilesSeen          int64    `json:"filesSeen"`
	DirsSeen           int64    `json:"dirsSeen"`
	BytesSeen          int64    `json:"bytesSeen"`
	FilesHashed        int64    `json:"filesHashed"`
	BytesHashed        int64    `json:"bytesHashed"`
	Skipped            int64    `json:"skipped"`  // symlinks, junctions, devices, etc. (never followed)
	Excluded           int64    `json:"excluded"` // matched an --exclude pattern or a default exclude
	TooSmall           int64    `json:"tooSmall"` // smaller than --min-size
	HardlinksCollapsed int64    `json:"hardlinksCollapsed"`
	ErrorCount         int64    `json:"errorCount"`
	Errors             []string `json:"errors,omitempty"` // first MaxStoredErrors messages
	DurationMs         int64    `json:"durationMs"`
}

// MaxStoredErrors caps how many individual error messages a report keeps, so
// scanning a folder full of permission-denied entries can't balloon the file.
const MaxStoredErrors = 200

// Report is the complete result of one scan.
type Report struct {
	Schema       int       `json:"schema"`
	Tool         string    `json:"tool"`
	Version      string    `json:"version"`
	CreatedAt    time.Time `json:"createdAt"`
	Mode         Mode      `json:"mode"`
	Roots        []string  `json:"roots"`
	MinSize      int64     `json:"minSize"`
	Excludes     []string  `json:"excludes,omitempty"`
	Folders      bool      `json:"folders"`
	Stats        Stats     `json:"stats"`
	FolderGroups []Group   `json:"folderGroups"`
	FileGroups   []Group   `json:"fileGroups"`
}

// Groups returns pointers to every group, folders first, so callers can loop
// over both kinds without caring which slice a group lives in.
func (r *Report) Groups() []*Group {
	out := make([]*Group, 0, len(r.FolderGroups)+len(r.FileGroups))
	for i := range r.FolderGroups {
		out = append(out, &r.FolderGroups[i])
	}
	for i := range r.FileGroups {
		out = append(out, &r.FileGroups[i])
	}
	return out
}

// Reclaimable totals the bytes that could be freed across every group.
func (r *Report) Reclaimable() int64 {
	var n int64
	for _, g := range r.Groups() {
		n += g.Reclaimable()
	}
	return n
}

// FindEntry looks up a copy by its report-wide ID. Returns nil, nil if no
// such ID exists (the caller must treat that as "refuse", never "ignore").
func (r *Report) FindEntry(id int) (*Group, *Entry) {
	for _, g := range r.Groups() {
		for i := range g.Entries {
			if g.Entries[i].ID == id {
				return g, &g.Entries[i]
			}
		}
	}
	return nil, nil
}

// Write saves the report as indented JSON at path.
//
// Side effects: creates/overwrites the file. It writes to a temporary file in
// the same folder first and then renames it into place, so a crash or full
// disk halfway through never leaves a truncated, unreadable report behind.
// Permissions are owner-only (0600) because the report lists private paths.
func Write(path string, r *Report) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	// os.CreateTemp creates the file with owner-only permissions (0600 on
	// Unix; on Windows it inherits the folder's ACL), which is what we want.
	tmp, err := os.CreateTemp(dir, ".twinsweep-report-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// If anything below fails, remove the half-written temp file.
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		// The write already failed; a Close error on top adds nothing, and
		// the deferred cleanup removes the partial file either way.
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	tmpName = "" // renamed successfully; nothing to clean up
	return nil
}

// Read loads a report previously saved by Write and sanity-checks it.
//
// Validation protects against loading the wrong JSON file or a damaged one:
// it must be a Twinsweep report of a schema we understand, every group must
// have at least two entries, entry IDs must be unique, and every path must be
// absolute (a relative path would be resolved against whatever folder clean
// happens to run in, which could point somewhere unexpected).
func Read(path string) (*Report, error) {
	// The path comes from the user's own --report argument; opening a file
	// they named is the point. Its contents are validated below and never
	// trusted for removals (cleanup re-verifies everything on disk).
	data, err := os.ReadFile(path) // #nosec G304 -- user-named report file, contents validated
	if err != nil {
		return nil, err
	}
	r, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return r, nil
}

// Parse decodes and validates a report from raw JSON bytes. Used by Read,
// and by the browser UI's "Load report" (which uploads the file's contents
// rather than a path). Applies every check described on Read.
func Parse(data []byte) (*Report, error) {
	var r Report
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("not a valid report: %w", err)
	}
	if r.Tool != ToolName {
		return nil, fmt.Errorf("not a Twinsweep report")
	}
	if r.Schema != SchemaVersion {
		return nil, fmt.Errorf("uses report schema %d; this version of Twinsweep reads schema %d", r.Schema, SchemaVersion)
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

// Validate checks the structural rules described on Read. It is also used by
// the web server on reports it builds itself, as a cheap consistency check.
func (r *Report) Validate() error {
	seen := map[int]bool{}
	for _, g := range r.Groups() {
		if g.Kind != KindFile && g.Kind != KindFolder {
			return fmt.Errorf("group %d has unknown kind %q", g.ID, g.Kind)
		}
		if len(g.Entries) < 2 {
			return fmt.Errorf("group %d has fewer than two copies", g.ID)
		}
		for _, e := range g.Entries {
			if seen[e.ID] {
				return fmt.Errorf("entry ID %d appears more than once", e.ID)
			}
			seen[e.ID] = true
			if !filepath.IsAbs(e.Path) {
				return fmt.Errorf("entry %d has a non-absolute path %q", e.ID, e.Path)
			}
		}
	}
	return nil
}
