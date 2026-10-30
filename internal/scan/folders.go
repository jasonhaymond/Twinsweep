// Duplicate-folder detection, plus the folder "signature" that cleanup uses
// to re-verify a folder right before removing it.
//
// A folder signature is a SHA-256 over the folder's sorted list of entries:
// each file's name, size, and content hash, and each subfolder's name and
// (recursively) signature. This is a "Merkle tree" - the same idea git uses -
// so two folders have equal signatures only if they contain the same names,
// arranged the same way, with byte-identical contents, all the way down.
// Folder timestamps are deliberately NOT part of it: a copied folder almost
// always has a new modification time but is still a duplicate.
//
// To avoid hashing every file on the disk, detection runs in two passes:
//   - "shape" signature: names + sizes only (no file reads). Folders whose
//     shape is unique can't be duplicates.
//   - "content" signature: computed only for folders that share a shape,
//     reading just the files inside them that aren't hashed yet.
package scan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"twinsweep/internal/report"
)

// sigEntry is one line of a folder signature.
type sigEntry struct {
	dir  bool
	name string
	size int64  // files only
	hash string // file content hash, or subfolder signature ("" in shape mode)
}

// digest computes a folder signature from its entries. Entries are sorted by
// name first so the result doesn't depend on the order the OS lists them.
// Names are written with %q (Go-quoted) so a name containing a newline or
// space can't be confused with the line format itself.
func digest(entries []sigEntry) string {
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	h := sha256.New()
	for _, e := range entries {
		if e.dir {
			fmt.Fprintf(h, "D %q %s\n", e.name, e.hash)
		} else {
			fmt.Fprintf(h, "F %q %d %s\n", e.name, e.size, e.hash)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// FolderSignature reads a folder from disk right now and returns its content
// signature (identical to what the scanner computes), the number of files
// inside, and their total size.
//
// Used by internal/clean to confirm, immediately before removing a folder,
// that it is still identical to the copy being kept. Any symlink, junction,
// or other special entry inside makes this fail: we refuse to remove a folder
// containing something we can't compare. Read-only.
func FolderSignature(ctx context.Context, path string) (sig string, files int, bytes int64, err error) {
	if err := ctx.Err(); err != nil {
		return "", 0, 0, err
	}
	list, err := os.ReadDir(path)
	if err != nil {
		return "", 0, 0, err
	}
	entries := make([]sigEntry, 0, len(list))
	for _, e := range list {
		full := filepath.Join(path, e.Name())
		t := e.Type()
		switch {
		case t.IsDir():
			sub, n, b, err := FolderSignature(ctx, full)
			if err != nil {
				return "", 0, 0, err
			}
			entries = append(entries, sigEntry{dir: true, name: e.Name(), hash: sub})
			files += n
			bytes += b
		case t.IsRegular():
			info, err := e.Info()
			if err != nil {
				return "", 0, 0, err
			}
			h, _, err := FullHash(ctx, full)
			if err != nil {
				return "", 0, 0, err
			}
			entries = append(entries, sigEntry{name: e.Name(), size: info.Size(), hash: h})
			files++
			bytes += info.Size()
		default:
			return "", 0, 0, fmt.Errorf("%s is a link or special file, so this folder can't be fully compared", full)
		}
	}
	return digest(entries), files, bytes, nil
}

// folderGroups finds groups of identical folders. Returns groups of folder
// indexes (into s.dirs), already reduced to the topmost duplicates.
func (s *scanner) folderGroups(ctx context.Context) ([][]int, error) {
	s.setPhase("folders")
	n := len(s.dirs)

	// Pass 1: shape signature + totals, children before parents (children
	// always have higher indexes - see walk). Incomplete folders are skipped
	// and, because incompleteness propagates upwards, every folder we do
	// compute only has complete children.
	for d := n - 1; d >= 0; d-- {
		dr := &s.dirs[d]
		if dr.incomplete {
			continue
		}
		entries := make([]sigEntry, 0, len(dr.files)+len(dr.dirs))
		for _, fi := range dr.files {
			f := &s.files[fi]
			entries = append(entries, sigEntry{name: f.name, size: f.size})
			dr.totalFiles++
			dr.totalBytes += f.size
		}
		for _, ci := range dr.dirs {
			c := &s.dirs[ci]
			entries = append(entries, sigEntry{dir: true, name: c.name, hash: c.shape})
			dr.totalFiles += c.totalFiles
			dr.totalBytes += c.totalBytes
		}
		dr.shape = digest(entries)
	}

	// Folders sharing a shape are candidates. Folders with no files at all
	// (empty, or only empty subfolders) are ignored: "duplicate empty
	// folders" free no space and are usually intentional structure.
	var all []int
	for d := 0; d < n; d++ {
		if !s.dirs[d].incomplete && s.dirs[d].totalFiles > 0 {
			all = append(all, d)
		}
	}
	cands := bucketsOf(all, func(d int) (string, bool) { return s.dirs[d].shape, true })
	if len(cands) == 0 {
		return nil, nil
	}

	var groups [][]int
	if s.opts.Mode == report.ModeFast {
		// Fast mode never reads contents; shape match is the answer.
		groups = cands
	} else {
		var err error
		groups, err = s.contentGroups(ctx, cands)
		if err != nil {
			return nil, err
		}
	}
	return s.topmost(groups), nil
}

// contentGroups computes content signatures for the candidate folders (and
// everything inside them) and regroups on them.
func (s *scanner) contentGroups(ctx context.Context, cands [][]int) ([][]int, error) {
	n := len(s.dirs)
	// Mark each candidate folder and all of its descendants.
	inTree := make([]bool, n)
	var mark func(d int)
	mark = func(d int) {
		if inTree[d] {
			return
		}
		inTree[d] = true
		for _, c := range s.dirs[d].dirs {
			mark(c)
		}
	}
	for _, g := range cands {
		for _, d := range g {
			mark(d)
		}
	}

	// Hash every file in those trees that the file stages didn't already
	// fully hash (unique-size files, files below --min-size, files whose
	// partial hash was unique).
	var need []int
	for d := 0; d < n; d++ {
		if !inTree[d] {
			continue
		}
		for _, fi := range s.dirs[d].files {
			if f := &s.files[fi]; f.hash == "" && !f.failed {
				need = append(need, fi)
			}
		}
	}
	if err := s.parallel(ctx, need, s.fullHashWork(ctx)); err != nil {
		return nil, err
	}

	// Content signatures, children before parents. A folder holding a file
	// that failed to read gets no signature ("") and so matches nothing.
	for d := n - 1; d >= 0; d-- {
		if !inTree[d] {
			continue
		}
		dr := &s.dirs[d]
		entries := make([]sigEntry, 0, len(dr.files)+len(dr.dirs))
		ok := true
		for _, fi := range dr.files {
			f := &s.files[fi]
			if f.failed || f.hash == "" {
				ok = false
				break
			}
			entries = append(entries, sigEntry{name: f.name, size: f.size, hash: f.hash})
		}
		for _, ci := range dr.dirs {
			if !ok || s.dirs[ci].content == "" {
				ok = false
				break
			}
			entries = append(entries, sigEntry{dir: true, name: s.dirs[ci].name, hash: s.dirs[ci].content})
		}
		if ok {
			dr.content = digest(entries)
		}
	}

	var groups [][]int
	for _, g := range cands {
		groups = append(groups, bucketsOf(g, func(d int) (string, bool) {
			return s.dirs[d].content, s.dirs[d].content != ""
		})...)
	}
	return groups, nil
}

// topmost removes groups that are implied by a group one level up. If
// "Backup1" and "Backup2" are duplicates, then "Backup1/Photos" and
// "Backup2/Photos" are too - but reporting both just repeats the same space.
// A group is dropped only when the parents of ALL its members are members of
// one single other group; e.g. {A/x, B/x, C/x} where only A and B match is
// kept, because C/x's copy isn't explained by any parent match.
// Each member must also have a DIFFERENT parent: "P/x" and "P/x-copy" side
// by side are a real duplicate pair even if P itself matches some Q.
func (s *scanner) topmost(groups [][]int) [][]int {
	groupOf := map[int]int{} // folder index -> group number
	for gi, g := range groups {
		for _, d := range g {
			groupOf[d] = gi
		}
	}
	var out [][]int
	for gi, g := range groups {
		implied := true
		parentGroup := -1
		parents := map[int]bool{}
		for _, d := range g {
			p := s.dirs[d].parent
			pg, ok := groupOf[p]
			if p < 0 || !ok || pg == gi || parents[p] || (parentGroup >= 0 && pg != parentGroup) {
				implied = false
				break
			}
			parents[p] = true
			parentGroup = pg
		}
		if !implied {
			out = append(out, g)
		}
	}
	return out
}
