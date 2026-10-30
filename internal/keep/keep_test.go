// Tests for the keep rules: each proves a rule picks the copy its name
// promises, and that preferred folders override the rule.
package keep

import (
	"path/filepath"
	"testing"
	"time"

	"twinsweep/internal/report"
)

// abs builds an absolute test path that is valid on any OS.
func abs(parts ...string) string {
	return filepath.Join(append([]string{string(filepath.Separator) + "data"}, parts...)...)
}

// sample returns three copies with different ages, depths and roots.
func sample() []report.Entry {
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	return []report.Entry{
		{ID: 1, Path: abs("b", "deep", "x.jpg"), ModTime: t0.Add(2 * time.Hour), Root: 1},
		{ID: 2, Path: abs("a", "x.jpg"), ModTime: t0, Root: 2},
		{ID: 3, Path: abs("c", "x.jpg"), ModTime: t0.Add(time.Hour), Root: 0},
	}
}

// TestStrategies proves each rule selects the expected copy.
func TestStrategies(t *testing.T) {
	cases := map[Strategy]int{Newest: 1, Oldest: 2, FirstRoot: 3, ShortestPath: 2}
	for st, wantID := range cases {
		e := sample()
		if got := e[Rule{Strategy: st}.Choose(e)].ID; got != wantID {
			t.Errorf("%s kept #%d, want #%d", st, got, wantID)
		}
	}
}

// TestPreferOverridesStrategy proves a --prefer folder wins even when the
// rule alone would pick a different copy.
func TestPreferOverridesStrategy(t *testing.T) {
	e := sample()
	r := Rule{Strategy: Oldest, Prefer: []string{abs("c")}}
	if got := e[r.Choose(e)].ID; got != 3 {
		t.Fatalf("kept #%d, want #3 (inside preferred folder)", got)
	}
}

// TestSelectKeepsOnePerGroup proves Select never selects every copy of a
// group: exactly len-1 removals per group.
func TestSelectKeepsOnePerGroup(t *testing.T) {
	rep := &report.Report{FileGroups: []report.Group{{ID: 1, Kind: report.KindFile, Entries: sample()}}}
	ids, err := Select(rep, Rule{Strategy: Newest}, "all")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("selected %d for removal, want 2", len(ids))
	}
	for _, id := range ids {
		if id == 1 {
			t.Fatal("selected the keeper for removal")
		}
	}
}

// TestUnknownStrategyRejected proves a typo in --keep is an error, not a
// silent fallback to some default rule.
func TestUnknownStrategyRejected(t *testing.T) {
	if _, err := ParseStrategy("biggest"); err == nil {
		t.Fatal("expected error for unknown rule")
	}
}
