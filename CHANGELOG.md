# Changelog

All notable changes to Twinsweep are documented here.
Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versioning: [SemVer](https://semver.org/).

Back to [README](README.md).

## [1.0.0] - 2026-10-30

First stable release: the command-line interface, report format (schema 1) and browser app are now
considered stable. No change to how Twinsweep scans or cleans compared with 0.2.0.

### Added
- Release binaries for Windows, macOS and Linux (x86-64 and ARM64) with `SHA256SUMS.txt` are published
  on the GitHub Releases page for each version.
- `scripts/release.ps1`: builds a version's binaries from its exact tag in a temporary checkout, checks
  the built version, and publishes them as a GitHub Release with that version's changelog as notes.
  Refuses tags without a recorded security-review approval or that haven't been pushed; `-DryRun` previews.
- PSScriptAnalyzer (PowerShell linter) over `scripts/` added to the security suite.

### Known limitations
- The macOS Recycle Bin/Trash path (via Finder) has not yet been verified on real Mac hardware.

## [0.2.0] - 2026-10-30

The review page is now a full browser app.

### Added
- `twinsweep ui` (alias of `serve`) opens the app with no arguments; folders or `--report` remain optional.
- Running `twinsweep` with no command (e.g. double-clicking it) opens the browser app. On a headless
  system (SSH session, or Linux/BSD with no `DISPLAY`/`WAYLAND_DISPLAY`) it prints help instead, with
  SSH-tunnel instructions. Options with no command (`twinsweep --port 8080`) go to the app.
- **Scan screen:** pick folders with a built-in folder browser (drives, home, subfolders) or by typing
  paths; reorder them by priority; set every scan option (exact/fast, duplicate folders, minimum size,
  exclude patterns, system-folder skipping, parallel reads); live progress with **Cancel**.
- **Open a saved report** from a file; reports are validated exactly like `--report`.
- **Review screen:** scan statistics and unreadable-item list, **Rescan**, **Download report** (JSON) and
  **Download CSV**, auto-select with multiple preferred folders and folders/files/all scope.
- **Cleanup progress dialog:** cleanup (and dry runs) run as a background job showing items and bytes
  done, the current item and step (checking kept copy / verifying / removing), running
  removed/refused/failed counts, recent results, and **Stop after current item**.
- **Activity screen:** the action log (what was removed, when, how, and which copy was kept).
- `clean.Options.Starting` progress hook and `clean.Check` (request validation without running).

### Changed
- `serve` no longer requires folders or a report.
- Quit is refused while a cleanup is running; a running scan is cancelled on Quit.

### Security
- New endpoints use the existing session, CSRF, Host-check and CSP protections; the folder browser
  lists folder names only (no files, no links) for the signed-in session; scan requests require
  absolute paths and are bounded (folders, patterns, workers); report uploads are capped at 256 MiB
  and fully validated; a second cleanup, a scan, or a report load is refused while a cleanup runs.
- Accepted risk: the folder picker can list folder names anywhere the user's own account can open
  (no base-folder confinement), because choosing scan folders anywhere is its purpose.

## [0.1.0] - 2026-10-30

First release.

### Added
- `twinsweep scan`: exact duplicate-file detection (size → head/tail hash → full SHA-256), duplicate-folder
  detection (Merkle signatures, topmost duplicates only, redundant file groups suppressed), and fast
  name+size mode. Text, JSON and CSV output; `--out` saves a report for later.
- Hardlinks collapsed (never reported as duplicates); symlinks, junctions and special files never followed;
  folders containing anything uncomparable are never offered as duplicate folders.
- Default excludes for OS trash/index folders; `--exclude` glob patterns; `--min-size`; nested scan
  folders de-duplicated so a file can't be a "duplicate of itself".
- `twinsweep clean`: keep rules (`newest`, `oldest`, `shortest-path`, `first-root`, plus `--prefer`),
  dry run by default, Recycle Bin/Trash by default, permanent delete only with
  `--action delete --apply --yes-permanently-delete`.
- Cleanup re-verification of every target and kept copy against the disk, refusal of anything changed,
  of hardlink/alias targets, and of any removal that would leave a group with no surviving copy.
- Recycle Bin/Trash support: Windows shell (fixed drives only), macOS Finder (supports Put Back),
  Linux/BSD freedesktop.org Trash spec including per-drive `.Trash-$UID`.
- JSON Lines action log with size-based rotation.
- `twinsweep serve`: local review page (127.0.0.1 only) with one-time sign-in link, session cookie,
  CSRF protections, Host-header (DNS-rebinding) checks, strict CSP, auto-select, dry-run preview,
  typed `DELETE` confirmation for permanent delete, light/dark themes and phone layout.
- Build script for Windows/macOS/Linux × amd64/arm64 with SHA-256 checksums; security-suite script.

### Security
- Go toolchain pinned to 1.27.2 (`toolchain` in go.mod) for net/http fixes GO-2026-6611/6612/6613/6617.
- Browser launcher on Windows locates `rundll32.exe` via `GetSystemDirectory`, not `%SystemRoot%`.
- Accepted risk: the review page's session cookie has no `Secure` flag (loopback-only plain HTTP;
  HttpOnly + SameSite=Strict).
