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
| `cmd/twinsweep/main.go` | Command-line parsing and output for `scan`, `clean`, `serve`, `version` |
| `internal/scan/` | Read-only walking, hashing, duplicate file + folder detection |
| `internal/keep/` | Keep rules (which copy of each group survives) |
| `internal/clean/` | The only code that removes anything: re-verification, removal, action log |
| `internal/trash/` | Recycle Bin / Trash per OS (`trash_windows.go`, `trash_darwin.go`, `trash_unix.go`) |
| `internal/web/` | Review page server + embedded `static/` HTML/JS/CSS |
| `internal/report/` | Report data model, JSON read/write, text and CSV output |
| `internal/pathutil/` | Path containment/equality helpers (safety-critical) |
| `internal/version/` | The single app version constant |
| `scripts/build.ps1` | Cross-compile all release binaries + checksums |
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
| `web` | 401 without session; one-time sign-in link; DNS-rebinding Host rejection; CSRF (origin, Sec-Fetch-Site, token, content type); security headers, no CORS; static path traversal; ID-only targeting; all-copies refusal; typed DELETE; body size limit |
| `trash` | Linux: freedesktop layout, URL-encoded `.trashinfo`, name collisions, symlinked `.Trash-$UID` rejected. Windows: network paths refused with `ErrNoTrash`; real Recycle Bin test opt-in via `TWINSWEEP_TEST_RECYCLE=1` |
| `keep`, `pathutil` | Each rule picks the right copy; `--prefer` wins; containment edge cases (`Data2` vs `Data`, roots, case folding) |

**Windows can't run the symlink tests** without Developer Mode, so they skip there. Run the Linux pass via
WSL before every release (no Go needed inside WSL):

```powershell
New-Item -ItemType Directory -Force "$env:TEMP\twl" | Out-Null
foreach ($p in 'clean','keep','pathutil','scan','trash','web') {
  $env:GOOS='linux'; $env:GOARCH='amd64'; go test -c -o "$env:TEMP\twl\$p.test" "./internal/$p"
}
Remove-Item Env:GOOS, Env:GOARCH
wsl -d Ubuntu -- sh -c 'mkdir -p /tmp/twl && cp /mnt/c/Users/$USER/AppData/Local/Temp/twl/*.test /tmp/twl/ 2>/dev/null; cd /tmp/twl && for t in *.test; do chmod +x $t; ./$t -test.count=1 | tail -1; done'
```

Expected: `PASS` for each package, with no `SKIP` lines when run with `-test.v`. (If your Windows and WSL
user names differ, adjust the `/mnt/c/Users/...` path.)

**Manual / UI checks** before calling a UI change done: run `twinsweep serve` on a test tree, check the
page at desktop width and at ~390 px wide (browser dev tools device mode), in light and dark mode, and
take screenshots. Also test the CLI failure paths: missing folder, unknown `--keep`, permanent delete
without the confirmation flag.

## 4. Security review gate

Every change runs through the full suite, with results shown to and **approved by Jason**, before it is
committed, tagged, or shared.

```powershell
pwsh ./scripts/security-check.ps1
```

It runs gofmt/vet, staticcheck, gosec, semgrep (if installed), govulncheck, gitleaks (if installed), the
full test suite, and an outdated-dependency check. Missing tools are reported **NOT RUN**, never hidden.
The report is written to `security-reports/v<version>-<commit>-<timestamp>.md` (gitignored).

Optional tools for full coverage:

```powershell
pip install --user semgrep   # SAST with OWASP rulesets
winget install gitleaks      # secret scanning (brew install gitleaks on macOS)
```

Both installers add themselves to PATH for *new* terminals only; open a fresh terminal before re-running
the suite, or they'll still show NOT RUN.

Approved suppressions are marked in the code with `#nosec <rule> -- reason` (gosec) or
`nosemgrep: <rule>` (semgrep). The only accepted risk so far is the review page's session cookie lacking
the `Secure` flag (loopback-only HTTP); see CHANGELOG 0.1.0.

`#nosec` annotations in the code each carry a reason (e.g. reading user-selected files is the tool's
purpose). Adding a new one, or weakening any test, must be called out in the report and approved.

## 5. Releasing a version

1. Finish the batch of changes; bump `internal/version/version.go` and add a `CHANGELOG.md` entry.
2. Run the security gate (above) and the Linux test pass; get approval.
3. Commit, then tag with the approval recorded:
   ```powershell
   git tag -a v0.1.0 -m "Twinsweep 0.1.0`n`nSecurity review: approved <date>, report <file>, accepted risks: none"
   git push origin main --tags
   ```
4. `pwsh ./scripts/build.ps1` and publish the contents of `dist/`.

Twinsweep has no server or database, so there is no deploy/rollback procedure; rolling back means using
an older release binary (`git checkout vX.Y.Z` then rebuild). Report files carry a schema number, and
older versions refuse reports with a schema they don't understand.

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
  AppleScript injection. Untested on real macOS hardware as of 0.1.0 — verify before relying on it.
- **Review page tokens:** a one-time bootstrap code in the printed URL is exchanged for a session cookie
  (httpOnly, SameSite=Strict, port-specific name). Mutating requests also need the session token in
  `X-Twinsweep-Token`, which the page reads from a `<meta>` tag.
