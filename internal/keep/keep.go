// Package keep decides, for each duplicate group, which ONE copy to keep.
// Every other copy becomes a removal candidate.
//
// Where it sits: `twinsweep clean --keep <rule>` and the "Auto-select" button
// on the review page both call Select. It only chooses; it never touches the
// disk. The cleanup engine (internal/clean) independently guarantees at least
// one copy survives, so a bug here could pick the "wrong" copy to keep but
// could never cause every copy to be removed.
package keep

import (
	"fmt"
	"strings"

	"twinsweep/internal/pathutil"
	"twinsweep/internal/report"
)

// Strategy names a keep rule.
type Strategy string

const (
	Newest       Strategy = "newest"        // keep the most recently modified copy
	Oldest       Strategy = "oldest"        // keep the oldest copy (often "the original")
	ShortestPath Strategy = "shortest-path" // keep the copy nearest the top of its folder tree
	FirstRoot    Strategy = "first-root"    // keep the copy under the earliest-listed scan folder
)

// Strategies lists every valid rule, for help text and input validation.
var Strategies = []Strategy{Newest, Oldest, ShortestPath, FirstRoot}

// ParseStrategy converts user input into a Strategy, rejecting unknown names.
func ParseStrategy(s string) (Strategy, error) {
	for _, st := range Strategies {
		if string(st) == s {
			return st, nil
		}
	}
	names := make([]string, len(Strategies))
	for i, st := range Strategies {
		names[i] = string(st)
	}
	return "", fmt.Errorf("unknown keep rule %q (choose one of: %s)", s, strings.Join(names, ", "))
}

// Rule is a strategy plus optional preferred folders. A copy inside an
// earlier-listed Prefer folder always wins over one that isn't; the strategy
// only breaks ties between copies with the same preference.
// Example: Prefer=["D:\Photos"], Strategy=oldest keeps whichever copy is in
// D:\Photos, and if none (or several) are, the oldest one.
type Rule struct {
	Strategy Strategy
	Prefer   []string
}

// preference returns the index of the first Prefer folder containing path,
// or len(Prefer) if none do (lower = more preferred).
func (r Rule) preference(path string) int {
	for i, p := range r.Prefer {
		if pathutil.IsUnder(path, p) {
			return i
		}
	}
	return len(r.Prefer)
}

// better reports whether copy a should be kept in preference to copy b.
// Final tie-breakers (shorter path, then alphabetical) make the choice
// deterministic: the same report and rule always pick the same copy.
func (r Rule) better(a, b report.Entry) bool {
	if pa, pb := r.preference(a.Path), r.preference(b.Path); pa != pb {
		return pa < pb
	}
	switch r.Strategy {
	case Newest:
		if !a.ModTime.Equal(b.ModTime) {
			return a.ModTime.After(b.ModTime)
		}
	case Oldest:
		if !a.ModTime.Equal(b.ModTime) {
			return a.ModTime.Before(b.ModTime)
		}
	case FirstRoot:
		if a.Root != b.Root {
			return a.Root < b.Root
		}
	case ShortestPath:
		// handled by the shared tie-breaker below
	}
	if len(a.Path) != len(b.Path) {
		return len(a.Path) < len(b.Path)
	}
	return a.Path < b.Path
}

// Choose returns the index (within entries) of the copy to keep.
// entries must not be empty.
func (r Rule) Choose(entries []report.Entry) int {
	best := 0
	for i := 1; i < len(entries); i++ {
		if r.better(entries[i], entries[best]) {
			best = i
		}
	}
	return best
}

// Select applies the rule to every group of the requested kinds and returns
// the entry IDs to remove (every copy except the chosen keeper).
// kinds: "all", "files" or "folders".
func Select(rep *report.Report, r Rule, kinds string) ([]int, error) {
	if _, err := ParseStrategy(string(r.Strategy)); err != nil {
		return nil, err
	}
	if kinds != "all" && kinds != "files" && kinds != "folders" {
		return nil, fmt.Errorf("unknown group kind %q (use all, files or folders)", kinds)
	}
	var ids []int
	for _, g := range rep.Groups() {
		if (kinds == "files" && g.Kind != report.KindFile) || (kinds == "folders" && g.Kind != report.KindFolder) {
			continue
		}
		if len(g.Entries) < 2 {
			continue
		}
		k := r.Choose(g.Entries)
		for i, e := range g.Entries {
			if i != k {
				ids = append(ids, e.ID)
			}
		}
	}
	return ids, nil
}
