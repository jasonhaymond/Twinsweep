# Twinsweep

Find and safely clean up duplicate files **and duplicate folders** on Windows, macOS and Linux.
One self-contained binary, no installer, no runtime, no network access.

**Current version: 1.0.1**. **Download:** [Releases page](https://github.com/jasonhaymond/Twinsweep/releases)
(Windows, macOS, Linux; x86-64 and ARM64). History: [CHANGELOG.md](CHANGELOG.md).

## Documentation

| Doc | For |
|---|---|
| [docs/user-guide.md](docs/user-guide.md) | Installing and using Twinsweep, step by step: scanning, reviewing in the browser, cleaning up, restoring |
| [docs/development.md](docs/development.md) | Building, testing, the security gate, releasing, and how the code is organised |
| [CHANGELOG.md](CHANGELOG.md) | Version history |

## What it does

- **Exact duplicate files** — byte-identical content, confirmed with SHA-256. Files are narrowed by size, then
  by a hash of their first and last 64 KiB, before any full read, so most files are never read at all.
- **Duplicate folders** — whole folder trees with the same names and identical contents, reported once
  (a copied 10,000-file "Photos" backup is one item, not 10,000).
- **Fast mode** (`--fast`) — matches by file name + size without reading contents, for huge or slow shares.
- **Browser app** (`twinsweep ui`) — everything from a local browser page: pick folders and scan, review
  results, download/open reports, auto-select (newest / oldest / shortest path / first scan folder, plus
  "prefer these folders"), clean up with live progress and a Stop button, and browse the activity log.
- **Cleanup** — to the **Recycle Bin / Trash** by default (restorable the normal way), or permanent delete
  behind an explicit confirmation.

## Safety model (short version)

- `scan` never changes anything. `clean` is a **dry run** unless you add `--apply`.
- Before removing a copy, Twinsweep **re-reads it from disk** and re-checks it against the copy being kept:
  still a real file, same size and timestamp, identical full hash. Anything changed since the scan is refused.
- It **never removes the last copy** of anything — including when the "kept" copy sits inside a folder
  that is also selected for removal.
- Symlinks, junctions and cloud placeholders are never followed. Hardlinks aren't reported as duplicates.
- On Windows, items on drives without a Recycle Bin (network shares, USB sticks) are **refused** rather than
  silently deleted permanently, which is what Windows itself would do.
- The browser app listens on 127.0.0.1 only and is protected by a one-time sign-in link, CSRF tokens,
  Host-header checks and a strict Content Security Policy.
- Every removal is recorded in an action log.

Details: [docs/user-guide.md — How Twinsweep keeps your files safe](docs/user-guide.md#7-how-twinsweep-keeps-your-files-safe).

## Quick start

**Browser app** (easiest):

```powershell
twinsweep        # or double-click twinsweep.exe; same as "twinsweep ui"
```

Your browser opens on the Scan screen: add folders, press **Start scan**, review, then remove.
(Over SSH or on a server with no desktop, a bare `twinsweep` prints the command-line help instead.)

**Command line:**

```powershell
# 1. Find duplicates and save a report (read-only)
twinsweep scan D:\Photos E:\Backup --out report.json

# 2a. Review and choose in your browser ...
twinsweep ui --report report.json

# 2b. ... or clean from the terminal: dry run first, then apply
twinsweep clean --report report.json --keep oldest --prefer D:\Photos
twinsweep clean --report report.json --keep oldest --prefer D:\Photos --apply
```

## Local development

Requires Go 1.27+ (Twinsweep's only dependency is `golang.org/x/sys`, the Go team's OS-call package).

```powershell
go test ./...                          # all tests
go build -o dist/twinsweep.exe ./cmd/twinsweep
pwsh ./scripts/build.ps1               # release binaries for all 6 OS/CPU targets (into dist/)
pwsh ./scripts/security-check.ps1      # full security suite (required before every commit)
pwsh ./scripts/release.ps1 -DryRun     # preview publishing a tagged version to GitHub Releases
```

See [docs/development.md](docs/development.md) for the full workflow.
