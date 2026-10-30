// Human-readable and spreadsheet output for a Report: the text summary that
// `twinsweep scan` prints, the CSV export (--format csv), and the byte-size
// formatter used everywhere sizes are shown.
package report

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// HumanBytes turns a byte count into a short string like "4.2 GB".
// Uses 1024-based units, which is what Windows Explorer and most file
// managers display, so the numbers match what the user sees elsewhere.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	// Find the largest unit (KB, MB, GB...) that keeps the number >= 1.
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// WriteText prints a summary followed by the largest `limit` groups (limit
// <= 0 means all). Output goes to w (normally the terminal).
func WriteText(w io.Writer, r *Report, limit int) {
	s := r.Stats
	fmt.Fprintf(w, "Twinsweep %s - %s mode scan of %d folder(s)\n", r.Version, r.Mode, len(r.Roots))
	for _, root := range r.Roots {
		fmt.Fprintf(w, "  %s\n", root)
	}
	fmt.Fprintf(w, "Scanned %d files (%s) in %d folders in %s\n",
		s.FilesSeen, HumanBytes(s.BytesSeen), s.DirsSeen, time.Duration(s.DurationMs)*time.Millisecond)
	fmt.Fprintf(w, "Hashed %d files (%s). Skipped %d links/special files, %d excluded, %d below min size, %d hardlinks already sharing storage.\n",
		s.FilesHashed, HumanBytes(s.BytesHashed), s.Skipped, s.Excluded, s.TooSmall, s.HardlinksCollapsed)
	if s.ErrorCount > 0 {
		fmt.Fprintf(w, "%d item(s) could not be read (see report errors). Folders containing them are never treated as duplicates.\n", s.ErrorCount)
	}
	fmt.Fprintf(w, "\nFound %d duplicate folder group(s) and %d duplicate file group(s). Reclaimable: %s\n",
		len(r.FolderGroups), len(r.FileGroups), HumanBytes(r.Reclaimable()))

	// Print folder groups first (they usually hold the most space), then files.
	printed := 0
	for _, g := range r.Groups() {
		if limit > 0 && printed >= limit {
			fmt.Fprintf(w, "\n... %d more group(s) not shown. Save a report with --out and use `twinsweep serve --report` to review them all.\n",
				len(r.FolderGroups)+len(r.FileGroups)-printed)
			break
		}
		printed++
		label := "files"
		if g.Kind == KindFolder {
			label = fmt.Sprintf("folders, %d files each", g.FileCount)
		}
		fmt.Fprintf(w, "\n[%d] %d %s, %s each, %s reclaimable\n", g.ID, len(g.Entries), label, HumanBytes(g.Size), HumanBytes(g.Reclaimable()))
		for _, e := range g.Entries {
			fmt.Fprintf(w, "    #%-6d %s  %s\n", e.ID, e.ModTime.Local().Format("2006-01-02 15:04"), e.Path)
		}
	}
}

// csvSafe neutralizes "CSV/formula injection": spreadsheet apps (Excel,
// LibreOffice) execute a cell as a formula if it starts with =, +, -, @, or
// a tab/carriage return. A file literally named "=HYPERLINK(...)" could
// otherwise run a formula when the user opens the export. Prefixing a single
// quote makes the spreadsheet show it as plain text.
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// WriteCSV writes one row per copy (entry), with its group's details
// repeated on each row so the file can be sorted/filtered in a spreadsheet.
func WriteCSV(w io.Writer, r *Report) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"group_id", "kind", "copies", "size_bytes", "reclaimable_bytes", "hash", "entry_id", "path", "modified"}); err != nil {
		return err
	}
	for _, g := range r.Groups() {
		for _, e := range g.Entries {
			row := []string{
				strconv.Itoa(g.ID), string(g.Kind), strconv.Itoa(len(g.Entries)),
				strconv.FormatInt(g.Size, 10), strconv.FormatInt(g.Reclaimable(), 10), g.Hash,
				strconv.Itoa(e.ID), csvSafe(e.Path), e.ModTime.Format(time.RFC3339),
			}
			if err := cw.Write(row); err != nil {
				return err
			}
		}
	}
	cw.Flush()
	return cw.Error()
}
