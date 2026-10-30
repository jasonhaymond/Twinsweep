// The action log: an append-only record of every removal Twinsweep performed
// (or attempted), one JSON object per line ("JSON Lines" format), so you can
// always answer "what did Twinsweep do to my files, and when?" - and find
// where to restore something from.
//
// Default location (per-user, not shared):
//
//	Windows: %AppData%\Twinsweep\actions.log
//	macOS:   ~/Library/Application Support/Twinsweep/actions.log
//	Linux:   ~/.config/Twinsweep/actions.log ($XDG_CONFIG_HOME respected)
//
// The file is owner-only (0600) because it lists private file paths. When it
// exceeds 10 MiB it is renamed to actions.log.1 (replacing any older .1) and
// a fresh log is started, so it can never grow without bound.
package clean

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"twinsweep/internal/report"
	"twinsweep/internal/version"
)

// maxLogBytes is the size at which the log is rotated.
const maxLogBytes = 10 << 20

// DefaultLogPath returns the per-user action log location described above,
// falling back to the system temp folder if no config folder exists.
func DefaultLogPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "Twinsweep", "actions.log")
}

// actionLog is an open log file.
type actionLog struct {
	f    *os.File
	path string
}

// openLog opens (creating if needed) the log for appending, rotating it
// first if it has grown past maxLogBytes.
func openLog(path string) (*actionLog, error) {
	if path == "" {
		path = DefaultLogPath()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if info, err := os.Stat(path); err == nil && info.Size() > maxLogBytes {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600) // #nosec G304 -- user-chosen log path
	if err != nil {
		return nil, err
	}
	return &actionLog{f: f, path: path}, nil
}

// logLine is the JSON shape of one log record.
type logLine struct {
	Time     time.Time   `json:"time"`
	Version  string      `json:"version"`
	Action   Action      `json:"action"`
	Result   string      `json:"result"` // "ok" or "error"
	Error    string      `json:"error,omitempty"`
	Kind     report.Kind `json:"kind"`
	Path     string      `json:"path"`
	Size     int64       `json:"size"`
	Hash     string      `json:"hash"`
	KeptPath string      `json:"keptPath"`
}

// write appends one record. A nil log (dry run) does nothing. Write errors
// are ignored deliberately: the removal has already happened at this point,
// and failing loudly here would only hide that fact from the user.
func (l *actionLog) write(a Action, g *report.Group, e report.Entry, kept, hash string, opErr error) {
	if l == nil {
		return
	}
	line := logLine{
		Time: time.Now().UTC(), Version: version.Version, Action: a, Result: "ok",
		Kind: g.Kind, Path: e.Path, Size: e.Size, Hash: hash, KeptPath: kept,
	}
	if opErr != nil {
		line.Result, line.Error = "error", opErr.Error()
	}
	b, err := json.Marshal(line)
	if err != nil {
		return
	}
	_, _ = l.f.Write(append(b, '\n'))
}

// Close closes the log file. Safe on a nil log.
func (l *actionLog) Close() {
	if l != nil {
		_ = l.f.Close()
	}
}
