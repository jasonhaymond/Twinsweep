# Twinsweep development guide

Back to [README](../README.md) · Related: [User guide](user-guide.md) · [Changelog](../CHANGELOG.md)

## Contents

1. [Project layout](#1-project-layout)
2. [Set up and build](#2-set-up-and-build)
3. [Testing](#3-testing)
4. [Security review gate](#4-security-review-gate)
5. [Releasing a version](#5-releasing-a-version)
6. [Design notes](#6-design-notes)

---

## 1. Project layout

| Path | What it is |
|---|---|
| `cmd/twinsweep/main.go` | Command-line parsing and output for `scan`, `clean`, `ui` (alias `serve`), `version`; `browser_*.go` open the browser per OS |
| `internal/scan/` | Read-only walking, hashing, duplicate file + folder detection |
| `internal/keep/` | Keep rules (which copy of each group survives) |
| `internal/clean/` | The only code that removes anything: re-verification, removal, action log |
| `internal/trash/` | Recycle Bin / Trash per OS (`trash_windows.go`, `trash_darwin.go`, `trash_unix.go`) |
| `internal/web/` | Browser app: `server.go` (security, routing), `jobs.go` (background scan + cleanup with progress), `api.go` (folder browser, report export/load, auto-select, activity log), `places_*.go` (picker starting points), embedded `static/` HTML/JS/CSS |
| `internal/report/` | Report data model, JSON read/write, text and CSV output |
| `internal/pathutil/` | Path containment/equality helpers (safety-critical) |
| `internal/version/` | The single app version constant |
| `scripts/build.ps1` | Cross-compile all release binaries + checksums into `dist/` (gitignored) |
| `scripts/release.ps1` | Build a tagged version's binaries from that exact tag and publish them as a GitHub Release |
| `scripts/security-check.ps1` | Full security suite + report |

Code is commented for a reader who knows systems but not Go: every file has a header, every function a
doc comment, and security-relevant code says what it protects against.

## 2. Set up and build

1. Install the current Go release from <https://go.dev/dl/> (1.27 or later). `go.mod` pins the
   toolchain to **1.27.2** (security fixes), and Go downloads it automatically on first build if your
   install is older. Check from inside the repo:
   ```
   go version
   ```
   Expected: `go version go1.27.2 …` or newer.
2. Install PowerShell 7 if you're on macOS/Linux and want the scripts (`brew install powershell`,
   or see Microsoft's docs). Windows 11 can use `winget install Microsoft.PowerShell`.
3. Build a local binary:
   ```powershell
   go build -o dist/twinsweep.exe ./cmd/twinsweep
   ./dist/twinsweep.exe version
   ```
4. Build all release binaries (Windows/macOS/Linux × amd64/arm64) plus `dist/SHA256SUMS.txt`:
   ```powershell
   pwsh ./scripts/build.ps1
   ```
   Expected: six `dist/twinsweep-<version>-<os>-<arch>` files and the checksum file.

Dependencies: only `golang.org/x/sys` (Go team; used for safe Windows API calls). Check currency with
`go list -m -u all` and update with `go get golang.org/x/sys@latest && go mod tidy`.

## 3. Testing

```powershell
go test ./...                  # everything, on this OS
go test -v ./internal/clean    # one package, verbose
```

All tests use real files in temporary folders (deleted afterwards) — nothing is mocked except the trash
destination in `internal/clean` tests (`Options.TrashFunc`), so they never touch your real Recycle Bin.

| Package | What its tests prove |
|---|---|
| `scan` | Correct grouping; same-size-different-content never grouped; partial-hash collisions caught; hardlinks collapsed; symlinks never followed; nested roots can't self-duplicate; folder grouping and topmost reduction; fast mode |
| `clean` | Dry run changes nothing; last copy never removed; unknown IDs rejected; modified/content-swapped/symlink-swapped targets refused; missing/changed keeper refused; hardlink-to-keeper refused; tampered report can't remove unique files; keeper-inside-removed-folder refused; permanent delete needs confirmation; cancellation stops |
| `web` | 401 without session; one-time sign-in link; DNS-rebinding Host rejection; CSRF (origin, Sec-Fetch-Site, token, content type); security headers, no CORS; static path traversal; ID-only targeting; all-copies refusal; typed DELETE; body size limit; CSRF + auth on every new endpoint (scan, cancel, load, clean, stop); scan validation (relative paths, bad size/mode/workers); folder browser lists folders only; report upload validation; export as attachment; cleanup job progress, Stop-between-items, and refusal of second cleanup / scan / Quit while running |
| `trash` | Linux: freedesktop layout, URL-encoded `.trashinfo`, name collisions, symlinked `.Trash-$UID` rejected. Windows: network paths refused with `ErrNoTrash`; real Recycle Bin test opt-in via `TWINSWEEP_TEST_RECYCLE=1` |
| `keep`, `pathutil` | Each rule picks the right copy; `--prefer` wins; containment edge cases (`Data2` vs `Data`, roots, case folding) |
| `cmd/twinsweep` | Headless detection per OS/session (bare `twinsweep` never tries a browser over SSH or without a display); headless bare run prints help; unknown commands still fail |

**Windows can't run the symlink tests** without Developer Mode, so they skip there. Run the Linux pass via
WSL before every release (no Go needed inside WSL):

```powershell
New-Item -ItemType Directory -Force "$env:TEMP\twl" | Out-Null
$pkgs = @{ clean = './internal/clean'; keep = './internal/keep'; pathutil = './internal/pathutil'
          scan = './internal/scan'; trash = './internal/trash'; web = './internal/web'; cmd = './cmd/twinsweep' }
foreach ($p in $pkgs.Keys) {
  $env:GOOS='linux'; $env:GOARCH='amd64'; go test -c -o "$env:TEMP\twl\$p.test" $pkgs[$p]
}
Remove-Item Env:GOOS, Env:GOARCH
wsl -d Ubuntu -- sh -c 'mkdir -p /tmp/twl && cp /mnt/c/Users/$USER/AppData/Local/Temp/twl/*.test /tmp/twl/ 2>/dev/null; cd /tmp/twl && for t in *.test; do chmod +x $t; ./$t -test.count=1 | tail -1; done'
```

Expected: `PASS` for each package, with no `SKIP` lines when run with `-test.v`. (If your Windows and WSL
user names differ, adjust the `/mnt/c/Users/...` path.)

**Manual / UI checks** before calling a UI change done: run `twinsweep ui` on a test tree, walk Scan → Review → cleanup → Activity, check the
page at desktop width and at ~390 px wide (browser dev tools device mode), in light and dark mode, and
take screenshots. Also test the CLI failure paths: missing folder, unknown `--keep`, permanent delete
without the confirmation flag.

## 4. Security review gate

Every change runs through the full suite, with results shown to and **approved by Jason**, before it is
committed, tagged, or shared.

```powershell
pwsh ./scripts/security-check.ps1
```

It runs gofmt/vet, staticcheck, gosec, semgrep (if installed), PSScriptAnalyzer over `scripts/` (if
installed), govulncheck, gitleaks (if installed), the full test suite, and an outdated-dependency check.
Missing tools are reported **NOT RUN**, never hidden.
The report is written to `security-reports/v<version>-<commit>-<timestamp>.md` (gitignored).

Optional tools for full coverage:

```powershell
pip install --user semgrep                         # SAST with OWASP rulesets
winget install gitleaks                            # secret scanning (brew install gitleaks on macOS)
Install-Module PSScriptAnalyzer -Scope CurrentUser # PowerShell linter for scripts/
```

Both installers add themselves to PATH for *new* terminals only; open a fresh terminal before re-running
the suite, or they'll still show NOT RUN.

Approved suppressions are marked in the code with `#nosec <rule> -- reason` (gosec) or
`nosemgrep: <rule>` (semgrep). Accepted risks so far: the browser app's session cookie lacks the `Secure`
flag (loopback-only HTTP; CHANGELOG 0.1.0), and the folder picker isn't confined to a base folder
(choosing scan folders anywhere is its purpose; CHANGELOG 0.2.0).

`#nosec` annotations in the code each carry a reason (e.g. reading user-selected files is the tool's
purpose). Adding a new one, or weakening any test, must be called out in the report and approved.

## 5. Releasing a version

Every release ends with its binaries published as a **GitHub Release** for the version tag. Binaries are
**not** committed to git (they'd grow the repository by ~55 MB per version, permanently); `dist/` stays
gitignored.

1. Finish the batch of changes; bump `internal/version/version.go` and add a `CHANGELOG.md` section
   headed `## [X.Y.Z] - YYYY-MM-DD` (the release script uses it as the release notes).
2. Run the security gate (above) and the Linux test pass; get approval.
3. Commit, re-run the suite against that commit, then tag with the approval recorded and push:
   ```powershell
   git tag -a v1.0.0 -m "Twinsweep 1.0.0`n`nSecurity review: approved <date>, report <file>, accepted risks: <n>"
   git push origin main
   git push origin v1.0.0
   ```
4. Preview the release (builds and verifies, publishes nothing):
   ```powershell
   pwsh ./scripts/release.ps1 -DryRun
   ```
   Expected: `tag OK: v1.0.0 -> <commit>, security approval recorded, pushed`, six binaries built,
   `build OK: twinsweep 1.0.0 (...)`, the file list with sizes, and the release notes.
5. Publish:
   ```powershell
   pwsh ./scripts/release.ps1
   ```
   Expected: `Published: https://github.com/jasonhaymond/Twinsweep/releases/tag/v1.0.0`. Open it and
   confirm the 6 binaries and `SHA256SUMS.txt` are attached.

What `release.ps1` enforces: it refuses a tag that isn't annotated, has no `Security review: approved`
line, or hasn't been pushed; it builds in a temporary checkout of the exact tag (never your working
folder); it checks the built binary reports the right version; it refuses to touch an existing release
unless `-Replace` is given. Needs `gh` signed in (`gh auth status`). To release an older tag, pass
`-Version X.Y.Z`.

Twinsweep has no server or database, so there is no deploy/rollback procedure; rolling back means using
an older release's binaries from the GitHub Releases page (or `git checkout vX.Y.Z` then rebuild). Report
files carry a schema number, and older versions refuse reports with a schema they don't understand.

## 6. Design notes

- **Exact pipeline:** size buckets → hardlink collapse → SHA-256 of first+last 64 KiB → full SHA-256.
  Files ≤128 KiB get their full hash in the partial stage.
- **Folder signatures** are a Merkle hash of sorted `(name, size, content hash)` / `(name, subfolder
  signature)` lines. A cheap name+size "shape" pass picks candidates before any extra hashing. Folders
  holding anything uncomparable (link, unreadable, excluded) are never candidates, nor are their parents.
- **Topmost reduction:** a folder group is dropped when every member's parent is a distinct member of one
  other group; file groups entirely inside reported duplicate folders are hidden.
- **Cleanup ordering:** folder groups first, then file groups; files inside a selected folder are handled
  with it. Survivors exclude anything inside a selected folder.
- **Windows Recycle Bin:** `SHFileOperationW` with `FOF_ALLOWUNDO | FOF_WANTNUKEWARNING`, gated on
  `GetDriveType == DRIVE_FIXED`. 64-bit only (the struct layout differs on 32-bit).
- **macOS:** Finder via `/usr/bin/osascript`, path passed as an argument (not interpolated) to avoid
  AppleScript injection. Untested on real macOS hardware as of 1.0.0 — verify before relying on it.
- **Browser app tokens:** a one-time bootstrap code in the printed URL is exchanged for a session cookie
  (httpOnly, SameSite=Strict, port-specific name). Mutating requests also need the session token in
  `X-Twinsweep-Token`, which the page reads from a `<meta>` tag.
