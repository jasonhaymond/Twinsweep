# Changelog

All notable changes to Twinsweep are documented here.
Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versioning: [SemVer](https://semver.org/).

Back to [README](README.md).

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
