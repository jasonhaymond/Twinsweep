// Command twinsweep is the program's entry point: it reads the command line
// and runs one of the subcommands below.
//
//	twinsweep scan  <folders...>  find duplicates, print/save a report (read-only)
//	twinsweep clean --report ...  remove duplicates chosen by a keep rule (dry run unless --apply)
//	twinsweep serve <folders...>  scan and open the review page in a browser
//	twinsweep version             print the version
//
// Where it sits: this file only parses arguments and prints results. The
// real work lives in internal/scan (finding), internal/keep (choosing),
// internal/clean (removing, with all safety checks), internal/trash (OS
// Recycle Bin/Trash) and internal/web (review page).
//
// Exit codes (useful in scripts): 0 = success, 1 = error, 2 = bad command
// line, 3 = cleanup finished but some items were refused or failed.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"twinsweep/internal/clean"
	"twinsweep/internal/keep"
	"twinsweep/internal/report"
	"twinsweep/internal/scan"
	"twinsweep/internal/version"
	"twinsweep/internal/web"
)

// Exit codes.
const (
	exitOK      = 0
	exitError   = 1
	exitUsage   = 2
	exitPartial = 3
)

const usage = `Twinsweep %s - find and clean up duplicate files and folders.

Usage:
  twinsweep scan  [options] <folder>...    Find duplicates (never changes anything)
  twinsweep clean --report FILE --keep RULE [options]
                                           Remove duplicates (dry run unless --apply)
  twinsweep serve [options] <folder>...    Scan, then review in your browser
  twinsweep serve --report FILE            Review a saved report in your browser
  twinsweep version

Run "twinsweep <command> -h" for that command's options.

Typical use:
  twinsweep scan D:\Photos E:\Backup --out report.json
  twinsweep clean --report report.json --keep oldest --prefer D:\Photos
  twinsweep clean --report report.json --keep oldest --prefer D:\Photos --apply
`

// main hands off to run so tests could call run with their own arguments
// and output streams.
func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches to a subcommand and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintf(stderr, usage, version.Version)
		return exitUsage
	}
	// signal.NotifyContext gives a context that is cancelled when the user
	// presses Ctrl+C (SIGINT) or the OS asks us to stop (SIGTERM), so scans
	// and cleanups stop cleanly between files instead of mid-operation.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "scan":
		return cmdScan(ctx, args[1:], stdout, stderr)
	case "clean":
		return cmdClean(ctx, args[1:], stdout, stderr)
	case "serve":
		return cmdServe(ctx, args[1:], stdout, stderr)
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "twinsweep %s (%s/%s)\n", version.Version, runtime.GOOS, runtime.GOARCH)
		return exitOK
	case "help", "-h", "--help":
		fmt.Fprintf(stdout, usage, version.Version)
		return exitOK
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		fmt.Fprintf(stderr, usage, version.Version)
		return exitUsage
	}
}

// ---------- shared flag helpers ----------

// multiFlag lets a flag be given more than once (--exclude a --exclude b).
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ", ") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// sizeFlag parses human sizes: "0", "512", "4K", "10M", "2G" (1024-based).
type sizeFlag int64

func (s *sizeFlag) String() string { return strconv.FormatInt(int64(*s), 10) }

// Set parses one size value; the suffix (K/M/G/T, optional trailing B) sets
// the multiplier.
func (s *sizeFlag) Set(v string) error {
	v = strings.TrimSuffix(strings.ToUpper(strings.TrimSpace(v)), "B")
	mult := int64(1)
	if n := len(v); n > 0 {
		switch v[n-1] {
		case 'K':
			mult = 1 << 10
		case 'M':
			mult = 1 << 20
		case 'G':
			mult = 1 << 30
		case 'T':
			mult = 1 << 40
		}
		if mult > 1 {
			v = v[:n-1]
		}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return fmt.Errorf("invalid size %q (examples: 0, 4K, 10M, 1G)", v)
	}
	*s = sizeFlag(n * mult)
	return nil
}

// scanFlags are the options shared by `scan` and `serve`.
type scanFlags struct {
	fast, noFolders, noDefaultExcl bool
	minSize                        sizeFlag
	excludes                       multiFlag
	workers                        int
}

// addScanFlags registers the shared scan options on a flag set.
func addScanFlags(fs *flag.FlagSet) *scanFlags {
	sf := &scanFlags{minSize: 1}
	fs.BoolVar(&sf.fast, "fast", false, "fast mode: match by file name + size only, without reading contents\n(cleanup still verifies contents before removing anything)")
	fs.BoolVar(&sf.noFolders, "no-folders", false, "don't look for duplicate folders (files only)")
	fs.Var(&sf.minSize, "min-size", "ignore files smaller than this (e.g. 0, 4K, 10M); default 1 skips empty files")
	fs.Var(&sf.excludes, "exclude", "skip files/folders matching this pattern, e.g. node_modules or *.tmp (repeatable)")
	fs.BoolVar(&sf.noDefaultExcl, "no-default-excludes", false, "also scan OS trash/index folders ($RECYCLE.BIN, .Trash, .Spotlight-V100, ...)")
	fs.IntVar(&sf.workers, "workers", 4, "files read in parallel (lower for spinning disks, higher for SSDs)")
	return sf
}

// options converts the flags + folder arguments into scan.Options.
func (sf *scanFlags) options(roots []string) scan.Options {
	mode := report.ModeExact
	if sf.fast {
		mode = report.ModeFast
	}
	return scan.Options{
		Roots: roots, Mode: mode, MinSize: int64(sf.minSize), Excludes: sf.excludes,
		DefaultExcludes: !sf.noDefaultExcl, Folders: !sf.noFolders, Workers: sf.workers,
	}
}

// parseInterspersed parses flags that may appear before OR after the folder
// arguments ("scan D:\Photos --out r.json"). Go's flag package normally
// stops at the first non-flag argument, so we parse repeatedly, collecting
// positional arguments as we go. Returns the positional arguments.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// isTerminal reports whether w is an interactive console, so progress lines
// aren't written into redirected output files.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// progressPrinter returns a scan progress callback that redraws one status
// line on stderr ("\r" returns to the start of the line).
func progressPrinter(stderr io.Writer) func(scan.Progress) {
	return func(p scan.Progress) {
		line := fmt.Sprintf("%-8s %d files, %s", p.Phase, p.FilesSeen, report.HumanBytes(p.BytesSeen))
		if p.HashTotal > 0 {
			line += fmt.Sprintf(" | compared %d/%d", p.HashDone, p.HashTotal)
		}
		fmt.Fprintf(stderr, "\r%-78s", line)
		if p.Phase == "done" {
			fmt.Fprint(stderr, "\r"+strings.Repeat(" ", 78)+"\r")
		}
	}
}

// ---------- scan ----------

// cmdScan implements `twinsweep scan`. Read-only.
func cmdScan(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sf := addScanFlags(fs)
	out := fs.String("out", "", "save the full report as JSON to this file (needed for clean / serve --report)")
	format := fs.String("format", "text", "what to print: text, json or csv")
	limit := fs.Int("limit", 20, "groups to list in text output (0 = all)")
	quiet := fs.Bool("quiet", false, "no progress line")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: twinsweep scan [options] <folder>...\n\nFinds duplicate files and folders. Never modifies anything.\n\nOptions:")
		fs.PrintDefaults()
	}
	roots, err := parseInterspersed(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(roots) == 0 {
		fs.Usage()
		return exitUsage
	}
	if *format != "text" && *format != "json" && *format != "csv" {
		fmt.Fprintf(stderr, "unknown --format %q (use text, json or csv)\n", *format)
		return exitUsage
	}
	opts := sf.options(roots)
	if !*quiet && isTerminal(stderr) {
		opts.Progress = progressPrinter(stderr)
	}
	warnNestedRoots(roots, stderr)

	rep, err := scan.Run(ctx, opts)
	if err != nil {
		fmt.Fprintln(stderr, "scan failed:", err)
		return exitError
	}
	if *out != "" {
		if err := report.Write(*out, rep); err != nil {
			fmt.Fprintln(stderr, "could not save report:", err)
			return exitError
		}
	}
	switch *format {
	case "json":
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
	case "csv":
		if err := report.WriteCSV(stdout, rep); err != nil {
			fmt.Fprintln(stderr, "csv:", err)
			return exitError
		}
	default:
		report.WriteText(stdout, rep, *limit)
		if *out != "" {
			fmt.Fprintf(stdout, "\nReport saved to %s\nNext: twinsweep serve --report %s   (review in browser)\n  or: twinsweep clean --report %s --keep oldest   (dry run)\n", *out, *out, *out)
		} else if len(rep.FileGroups)+len(rep.FolderGroups) > 0 {
			fmt.Fprintln(stdout, "\nTip: add --out report.json to save this scan for `clean` or `serve --report`.")
		}
	}
	return exitOK
}

// warnNestedRoots tells the user when a folder they listed was dropped
// because it's inside another listed folder (see scan.NormalizeRoots).
func warnNestedRoots(roots []string, stderr io.Writer) {
	norm, err := scan.NormalizeRoots(roots)
	if err == nil && len(norm) < len(roots) {
		fmt.Fprintf(stderr, "note: %d folder(s) are inside other listed folders and are scanned once, as part of their parent.\n", len(roots)-len(norm))
	}
}

// ---------- clean ----------

// cmdClean implements `twinsweep clean`: dry run by default, removes only
// with --apply, permanent delete only with --action delete AND
// --yes-permanently-delete.
func cmdClean(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("clean", flag.ContinueOnError)
	fs.SetOutput(stderr)
	reportPath := fs.String("report", "", "report file saved by `twinsweep scan --out` (required)")
	keepRule := fs.String("keep", "", "which copy to keep in each group: newest, oldest, shortest-path, first-root (required)")
	var prefer multiFlag
	fs.Var(&prefer, "prefer", "always keep the copy inside this folder when a group has one (repeatable; earlier wins)")
	only := fs.String("only", "all", "which groups to clean: all, files or folders")
	action := fs.String("action", "trash", "trash = move to Recycle Bin/Trash (recoverable); delete = permanent")
	apply := fs.Bool("apply", false, "actually remove (without this, it's a dry run that only shows what would happen)")
	yes := fs.Bool("yes-permanently-delete", false, "required together with --action delete --apply")
	logPath := fs.String("log", "", "action log file (default: "+clean.DefaultLogPath()+")")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: twinsweep clean --report FILE --keep RULE [options]\n\nKeeps one copy per duplicate group and removes the rest. Every copy is re-checked\nagainst the disk first; anything that changed since the scan is left alone.\n\nOptions:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *reportPath == "" || *keepRule == "" || fs.NArg() > 0 {
		fs.Usage()
		return exitUsage
	}
	strategy, err := keep.ParseStrategy(*keepRule)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	act := clean.Action(*action)
	if act != clean.Trash && act != clean.Delete {
		fmt.Fprintf(stderr, "unknown --action %q (use trash or delete)\n", *action)
		return exitUsage
	}
	if act == clean.Delete && *apply && !*yes {
		fmt.Fprintln(stderr, "Permanent delete cannot be undone. Add --yes-permanently-delete to confirm, or use --action trash.")
		return exitUsage
	}
	// --prefer folders are compared as absolute paths.
	for i, p := range prefer {
		if a, err := filepath.Abs(p); err == nil {
			prefer[i] = a
		}
	}

	rep, err := report.Read(*reportPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	ids, err := keep.Select(rep, keep.Rule{Strategy: strategy, Prefer: prefer}, *only)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	if len(ids) == 0 {
		fmt.Fprintln(stdout, "Nothing to clean.")
		return exitOK
	}
	if age := time.Since(rep.CreatedAt); age > 24*time.Hour {
		fmt.Fprintf(stderr, "note: this report is %s old; anything changed since then will be refused. Consider re-scanning.\n", age.Round(time.Hour))
	}
	if !*apply {
		fmt.Fprintln(stdout, "DRY RUN - nothing will be changed. Add --apply to actually remove.")
	}

	verb := map[clean.Status]string{
		clean.WouldRemove: "WOULD REMOVE", clean.Removed: strings.ToUpper(string(act)) + "ED",
		clean.Refused: "REFUSED", clean.Failed: "FAILED", clean.InFolder: "WITH FOLDER",
	}
	if act == clean.Trash {
		verb[clean.Removed] = "TRASHED"
	}
	res, err := clean.Run(ctx, rep, ids, clean.Options{
		Action: act, Apply: *apply, ConfirmPermanent: *yes, LogPath: *logPath,
		Progress: func(o clean.Outcome) {
			line := fmt.Sprintf("%-12s %s (%s)", verb[o.Status], o.Path, report.HumanBytes(o.Size))
			if o.Reason != "" {
				line += " - " + o.Reason
			}
			fmt.Fprintln(stdout, line)
		},
	})
	if res == nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	if *apply {
		fmt.Fprintf(stdout, "\nRemoved %d item(s), freed %s. Refused %d, failed %d.\nAction log: %s\n",
			res.Removed, report.HumanBytes(res.BytesFreed), res.Refused, res.Failed, res.LogPath)
	} else {
		fmt.Fprintf(stdout, "\nDry run: %d item(s) would be removed, freeing %s. %d would be refused.\n",
			res.WouldRemove, report.HumanBytes(res.BytesWouldFree), res.Refused)
	}
	if err != nil {
		fmt.Fprintln(stderr, "stopped early:", err)
		return exitError
	}
	if res.Refused > 0 || res.Failed > 0 {
		return exitPartial
	}
	return exitOK
}

// ---------- serve ----------

// cmdServe implements `twinsweep serve`: starts the review server on
// 127.0.0.1 only, prints a one-time sign-in link, and opens the browser.
func cmdServe(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sf := addScanFlags(fs)
	reportPath := fs.String("report", "", "review a saved report instead of scanning")
	port := fs.Int("port", 0, "port on 127.0.0.1 to listen on (0 = pick a free one)")
	noBrowser := fs.Bool("no-browser", false, "don't open the browser automatically; just print the link")
	logPath := fs.String("log", "", "action log file (default: "+clean.DefaultLogPath()+")")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: twinsweep serve [options] <folder>...\n       twinsweep serve --report FILE\n\nOpens a review page (on this computer only) to choose which copies to remove.\n\nOptions:")
		fs.PrintDefaults()
	}
	roots, err := parseInterspersed(fs, args)
	if err != nil {
		return exitUsage
	}
	if (*reportPath == "") == (len(roots) == 0) {
		fs.Usage()
		return exitUsage
	}

	cfg := web.Config{LogPath: *logPath}
	if *reportPath != "" {
		if cfg.Report, err = report.Read(*reportPath); err != nil {
			fmt.Fprintln(stderr, err)
			return exitError
		}
	} else {
		cfg.ScanOpts = sf.options(roots)
		if _, err := scan.NormalizeRoots(roots); err != nil {
			fmt.Fprintln(stderr, err)
			return exitError
		}
		warnNestedRoots(roots, stderr)
	}

	// Listen on the loopback address ONLY - the page is never reachable
	// from the network, even if the firewall is off.
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(*port)))
	if err != nil {
		fmt.Fprintln(stderr, "could not start the review server:", err)
		return exitError
	}
	srv, err := web.New(cfg, ln.Addr().(*net.TCPAddr).Port)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	srv.StartScan(ctx)
	// ReadHeaderTimeout stops a client that opens a connection and never
	// finishes sending headers from tying the server up ("slowloris").
	httpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(stderr, "server error:", err)
		}
	}()

	link := srv.SignInURL()
	fmt.Fprintf(stdout, "Twinsweep review page: %s\n", link)
	fmt.Fprintln(stdout, "This link works once. Press Enter here for a fresh link; Ctrl+C or the page's Quit button to stop.")
	if !*noBrowser {
		if err := openBrowser(link); err != nil {
			fmt.Fprintln(stderr, "could not open a browser automatically; open the link above yourself:", err)
		}
	}
	// Each Enter in the terminal issues a new one-time link.
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			if u, err := srv.NewSignInURL(); err == nil {
				fmt.Fprintf(stdout, "New one-time link: %s\n", u)
			}
		}
	}()

	select {
	case <-ctx.Done():
	case <-srv.Done():
	}
	fmt.Fprintln(stdout, "Stopping.")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutCtx)
	return exitOK
}
