# Twinsweep user guide

Back to [README](../README.md) · Related: [Development guide](development.md) · [Changelog](../CHANGELOG.md)

This guide walks through installing Twinsweep, finding duplicates, choosing what to remove, removing it
safely, and getting things back if you change your mind. Every step shows the exact command and what you
should see.

## Contents

1. [Install](#1-install)
2. [Scan from the terminal](#2-scan-from-the-terminal)
3. [Use the browser app (scan, review, clean up)](#3-use-the-browser-app-scan-review-clean-up)
4. [Clean up from the terminal](#4-clean-up-from-the-terminal)
5. [Restore something you removed](#5-restore-something-you-removed)
6. [Option reference](#6-option-reference)
7. [How Twinsweep keeps your files safe](#7-how-twinsweep-keeps-your-files-safe)
8. [Troubleshooting](#8-troubleshooting)

---

## 1. Install

Twinsweep is a single program file. There is no installer and nothing else to install.

1. Open the [Releases page](https://github.com/jasonhaymond/Twinsweep/releases), pick the latest version,
   and download the file for your computer (plus `SHA256SUMS.txt` for step 2):

   | Computer | File |
   |---|---|
   | Windows (most PCs) | `twinsweep-1.0.0-windows-amd64.exe` |
   | Windows on ARM (Surface Pro X, Copilot+ PCs) | `twinsweep-1.0.0-windows-arm64.exe` |
   | Mac with Apple silicon (M1 and later) | `twinsweep-1.0.0-darwin-arm64` |
   | Intel Mac | `twinsweep-1.0.0-darwin-amd64` |
   | Linux PC / server | `twinsweep-1.0.0-linux-amd64` |
   | Linux on ARM (Raspberry Pi 4/5 64-bit) | `twinsweep-1.0.0-linux-arm64` |

2. (Recommended) Check the download wasn't corrupted. Compare the output with the matching line in
   `SHA256SUMS.txt`:

   ```powershell
   # Windows PowerShell
   Get-FileHash .\twinsweep-1.0.0-windows-amd64.exe -Algorithm SHA256
   ```
   ```sh
   # macOS / Linux (run in the folder containing both files)
   sha256sum -c SHA256SUMS.txt --ignore-missing
   ```
   Success looks like matching hex strings, or `twinsweep-…: OK`.

3. Rename it to `twinsweep` (`twinsweep.exe` on Windows) and put it somewhere convenient.

   - **Windows:** e.g. `C:\Tools\twinsweep.exe`. To run it from anywhere, add `C:\Tools` to your PATH
     (Start → "Edit environment variables for your account" → Path → New).
   - **macOS / Linux:**
     ```sh
     chmod +x twinsweep
     sudo mv twinsweep /usr/local/bin/
     ```
   - **macOS only:** the binary isn't notarized by Apple, so the first run is blocked by Gatekeeper.
     Clear the "downloaded from the internet" flag once:
     ```sh
     xattr -d com.apple.quarantine /usr/local/bin/twinsweep
     ```

4. Confirm it runs:

   ```
   twinsweep version
   ```
   You should see something like `twinsweep 1.0.0 (windows/amd64)`.

---

## 2. Scan from the terminal

Prefer clicking to typing? Skip to [section 3](#3-use-the-browser-app-scan-review-clean-up) — the browser
app does everything below too.

Scanning **only reads** files. It never moves, changes, or deletes anything.

1. Run a scan of one or more folders and save the result:

   ```powershell
   twinsweep scan D:\Photos E:\Backup --out report.json
   ```

   - List folders **in order of importance** if you plan to use the "first scan folder" keep rule.
   - If you list a folder and also a folder inside it, the inner one is just scanned once as part of the
     outer one (you'll see a `note:` saying so).

2. While it runs, a progress line shows files found and how many have been compared.

3. When it finishes you get a summary like:

   ```
   Scanned 48211 files (212.4 GB) in 3920 folders in 1m42s
   Hashed 9133 files (31.0 GB). Skipped 12 links/special files, 3 excluded, 140 below min size, 0 hardlinks already sharing storage.

   Found 4 duplicate folder group(s) and 812 duplicate file group(s). Reclaimable: 27.9 GB

   [1] 2 folders, 6120 files each, 18.2 GB each, 18.2 GB reclaimable
       #1      2023-04-02 10:15  E:\Backup\Photos
       #2      2021-11-19 08:03  D:\Photos
   ...
   Report saved to report.json
   ```

   - **Folder groups** are whole folders that are identical all the way down. Files inside them aren't
     listed again separately.
   - **Reclaimable** is the space freed by keeping one copy of everything and removing the rest.
   - "Skipped … links/special files": symbolic links, Windows junctions, and cloud-only placeholders
     (OneDrive/Nextcloud "online-only" files) are never read or removed.

**Useful variations**

```powershell
# Skip build folders and temp files
twinsweep scan D:\Projects --exclude node_modules --exclude "*.tmp" --out report.json

# Only consider files of 1 MB or more (much faster on big trees)
twinsweep scan D:\ --min-size 1M --out report.json

# Huge network share: compare by name + size only (contents are still checked before any removal)
twinsweep scan \\nas\archive --fast --out report.json

# Spreadsheet of every duplicate
twinsweep scan D:\Photos --format csv > duplicates.csv
```

> **Keep reports private.** A report lists full file paths. It is saved readable only by you.

---

## 3. Use the browser app (scan, review, clean up)

The browser app does everything — scanning, reviewing, saving/opening reports, removing duplicates with
live progress, and showing the activity log. It runs only on your computer; nothing is sent anywhere.

### 3.1 Start it

1. Run (or on Windows, just double-click `twinsweep.exe`):

   ```powershell
   twinsweep
   ```

   A bare `twinsweep` is the same as `twinsweep ui`, and any options given without a command go to the
   app (e.g. `twinsweep --port 8080`). On a system with **no desktop session** — an SSH session, or a
   Linux/BSD machine with no graphical display — it prints the command-line help instead. To use the app
   on such a server from your own computer, start it with a fixed port and tunnel to it:

   ```sh
   # on the server
   twinsweep ui --no-browser --port 8080
   # on your computer, in another terminal
   ssh -L 8080:127.0.0.1:8080 you@server
   ```

   then open the printed link in your local browser (it uses 127.0.0.1:8080, which the tunnel forwards).

   Variations: `twinsweep ui D:\Photos E:\Backup` starts scanning those folders straight away;
   `twinsweep ui --report report.json` opens a saved report. (`twinsweep serve` is the same command.)

2. Your browser opens automatically. The terminal shows:

   ```
   Twinsweep is open at: http://127.0.0.1:52811/?t=4f1c…
   This link works once. Press Enter here for a fresh link; Ctrl+C or the page's Quit button to stop.
   ```

   The link **only works once** (a security measure). To open the app in another browser, press
   **Enter** in the terminal for a new link. If the browser didn't open, copy the link into it yourself.
   Keep the terminal window open while you use the app.

3. The header has three screens: **Scan**, **Review** (with a count of duplicate groups) and **Activity**.

### 3.2 Scan

1. On the **Scan** screen, add folders under **Folders to scan**:
   - Click **Browse…**, click into folders (start from a drive or *Home*; **↑ Up** goes back), then click
     **Add this folder** to add the folder you're in. Or
   - Type a full path (e.g. `D:\Photos` or `/home/me/Photos`) and press **Enter** or **Add**.

   Use **↑ / ↓** to set priority order (used by the *copy in the first scan folder* keep rule) and **✕** to
   remove a folder.

2. Set **Options** if needed (defaults suit most cases):

   | Option | Meaning |
   |---|---|
   | Exact / Fast | Exact compares contents. Fast matches name + size only (good for huge network shares); contents are still checked before anything is removed. |
   | Duplicate folders | Also find whole identical folder trees. |
   | Skip Recycle Bin / Trash… | Don't scan OS trash and search-index folders. |
   | Ignore files smaller than | e.g. `1` (skip empty files), `1M` (much faster on big drives). |
   | Files read in parallel | 1–2 for spinning disks/NAS, 8+ for fast SSDs. |
   | Skip names matching | One pattern per line, e.g. `node_modules`, `*.tmp`. |

3. Click **Start scan**. A progress panel shows files found and files compared. **Cancel scan** stops it
   (your previous results, if any, are kept).

4. When it finishes, the app switches to **Review** automatically.

To reopen an earlier scan instead, click **Open a saved report…** and pick a `.json` report (one saved with
**Download report** or `twinsweep scan --out`).

### 3.3 Review

1. The top card shows **reclaimable space**, what was scanned, and counts. If some items couldn't be read,
   expand the red line to see which. Buttons here:
   - **Rescan** — repeat the same scan (do this if files changed since).
   - **Download report** — save the results as JSON to reopen later or use with `twinsweep clean`.
   - **Download CSV** — a spreadsheet of every duplicate.
2. **Folders / Files tabs** switch between duplicate folders and duplicate files. **Filter by path**
   narrows the list, e.g. type `Downloads`.
3. Choose what to remove:
   - **Tick** a copy to mark it for removal (its row turns orange and says *remove*). Unticked copies say
     *keep*. You can't tick the last copy in a group.
   - **Keep only this** on a row ticks every other copy in that group.
   - **Auto-select copies to remove** (click to expand): choose which copy to keep (*shortest path*,
     *oldest*, *newest*, *copy in the first scan folder*), whether to apply it to folders, files or both,
     and optionally list folders whose copies should always be kept (one per line). Click **Auto-select**.
4. The bar at the bottom shows how many copies are selected and how much space they use.

### 3.4 Remove duplicates (with progress)

1. Click **Preview (dry run)** first. A progress dialog re-checks every ticked copy against the disk and
   then lists what *would* happen — nothing is changed. Read any **refused** rows; the reason says why
   (e.g. "has been modified since the scan"). Click **Close**.
2. Click **Move to Recycle Bin** (Windows) / **Move to Trash** (macOS/Linux) and confirm.
3. The progress dialog shows:
   - a bar and "*N of M items · X of Y*" (by size, so a big folder moves the bar more than a small file);
   - the item being worked on and the step: *checking the kept copy*, *verifying*, or *removing*;
   - running counts of removed / refused / failed, and the latest results.

   **Stop after current item** stops the run safely — the item in progress finishes, nothing after it is
   touched.
4. When it's done, the dialog lists every item's result and the action log location. Click **Close**;
   removed groups disappear from the Review list.
5. (Only if you're sure) **Delete permanently…** asks you to type `DELETE`, then runs with the same
   progress dialog. Permanently deleted items do **not** go to the Recycle Bin/Trash.

While a cleanup runs you can't start a scan, open a report, or Quit — stop the cleanup first.

### 3.5 Activity and quitting

- The **Activity** screen lists recent removals (newest first): when, how, result, the item, its size and
  which copy was kept. **Refresh** reloads it.
- Click **Quit** (top right) or press Ctrl+C in the terminal when done.

---

## 4. Clean up from the terminal

Use this for scripting or when you already know the rule you want.

1. **Dry run first** (the default — nothing is changed):

   ```powershell
   twinsweep clean --report report.json --keep oldest --prefer D:\Photos
   ```

   Output lists each decision, then a summary:

   ```
   DRY RUN - nothing will be changed. Add --apply to actually remove.
   WOULD REMOVE E:\Backup\Photos (18.2 GB)
   WOULD REMOVE E:\Backup\Docs\report (1).txt (18 B)
   REFUSED      E:\Backup\Docs\report (2).txt (18 B) - has been modified since the scan

   Dry run: 2 item(s) would be removed, freeing 18.2 GB. 1 would be refused.
   ```

2. When the dry run looks right, add `--apply` to move them to the Recycle Bin/Trash:

   ```powershell
   twinsweep clean --report report.json --keep oldest --prefer D:\Photos --apply
   ```

   Success ends with `Removed N item(s), freed X. Refused 0, failed 0.` and the action log path.

3. Permanent delete requires **both** flags (and cannot be undone):

   ```powershell
   twinsweep clean --report report.json --keep oldest --action delete --apply --yes-permanently-delete
   ```

**Keep rules** (one copy per group is kept; ties are broken by shorter path, then alphabetically):

| `--keep` | Keeps |
|---|---|
| `oldest` | The copy with the oldest modification time — usually the original |
| `newest` | The most recently modified copy |
| `shortest-path` | The copy nearest the top of its folder tree (`D:\Photos\a.jpg` over `D:\Photos\old\copy\a.jpg`) |
| `first-root` | The copy under the scan folder listed first on the `scan` command line |

`--prefer FOLDER` (repeatable) overrides the rule: a copy inside a preferred folder always wins.
`--only files` / `--only folders` limits cleanup to one kind of group.

**Exit codes** for scripts: `0` success, `1` error, `2` bad command line, `3` finished but some items were
refused or failed.

---

## 5. Restore something you removed

- **Windows:** open the **Recycle Bin**, right-click the item → **Restore**.
- **macOS:** open the **Trash**, right-click → **Put Back**.
- **Linux:** open **Trash** in your file manager (Files, Dolphin, Thunar…) → **Restore**. Items from other
  drives are in that drive's own trash, which file managers show in the same Trash view.

Permanently deleted items can't be restored by Twinsweep.

The **action log** records every removal: what, when, how, and which copy was kept. It is one JSON object
per line:

| OS | Location |
|---|---|
| Windows | `%AppData%\Twinsweep\actions.log` |
| macOS | `~/Library/Application Support/Twinsweep/actions.log` |
| Linux | `~/.config/Twinsweep/actions.log` |

```powershell
# Windows: show the last 10 actions
Get-Content "$env:APPDATA\Twinsweep\actions.log" -Tail 10
```

---

## 6. Option reference

Run `twinsweep <command> -h` for the built-in list.

**scan / ui**

| Option | Meaning |
|---|---|
| `--out FILE` | (scan) Save the full report as JSON |
| `--format text\|json\|csv` | (scan) What to print (default `text`) |
| `--limit N` | (scan) Groups listed in text output (default 20, `0` = all) |
| `--fast` | Match by name + size only, without reading contents |
| `--no-folders` | Don't look for duplicate folders |
| `--min-size SIZE` | Ignore smaller files: `0`, `4K`, `10M`, `1G` (default `1` = skip empty files) |
| `--exclude PATTERN` | Skip matching names, e.g. `node_modules`, `*.tmp`; a pattern containing `\` or `/` must match the whole path. Repeatable |
| `--no-default-excludes` | Also scan Recycle Bin / Trash / search-index folders |
| `--workers N` | Files read in parallel (default 4; use 1–2 for spinning disks, 8+ for fast SSDs) |
| `--quiet` | (scan) No progress line |
| `--report FILE` | (ui) Review a saved report instead of scanning |
| `--port N` | (ui) Port on 127.0.0.1 (default: any free port) |
| `--no-browser` | (ui) Just print the link |
| `--log FILE` | (ui) Action log location |

**clean**

| Option | Meaning |
|---|---|
| `--report FILE` | Report from `scan --out` (required) |
| `--keep RULE` | `oldest`, `newest`, `shortest-path`, `first-root` (required) |
| `--prefer FOLDER` | Always keep the copy in this folder. Repeatable; earlier wins |
| `--only all\|files\|folders` | Which groups to clean (default `all`) |
| `--action trash\|delete` | Recycle Bin/Trash (default) or permanent |
| `--apply` | Actually remove (otherwise dry run) |
| `--yes-permanently-delete` | Required with `--action delete --apply` |
| `--log FILE` | Action log location |

**Default excludes:** `$RECYCLE.BIN`, `System Volume Information`, `.Trashes`, `.Trash`, `.Trash-*`,
`.Spotlight-V100`, `.fseventsd`, `.DocumentRevisions-V100`, `.TemporaryItems`.

---

## 7. How Twinsweep keeps your files safe

- **Scanning is read-only.** Only `clean --apply` and the browser app's remove buttons change anything.
- **Reports are hints, not orders.** Right before removing each copy, Twinsweep re-reads it and the copy
  it's keeping, and refuses if: the copy changed (size, timestamp, or content), it's now a link, it's the
  same physical file as the kept copy (a hardlink or a differently-spelled path), or the kept copy is
  missing or changed. A stale or edited report can therefore only ever remove a *real* duplicate.
- **At least one copy always survives** — including the tricky case where the copy you're keeping in one
  group is inside a folder you're removing in another.
- **Fast-mode matches are verified by content** before removal, so a same-name-same-size false match is
  refused rather than removed.
- **No silent permanent deletes.** On Windows, the Recycle Bin is only used on internal fixed drives;
  network and removable drives are refused with a clear message (Windows would otherwise delete them
  permanently without telling you). On Linux, if no usable Trash exists for a drive, the item is refused.
- **Links are never followed** — a symlink or junction can't lead a scan or a removal outside the folders
  you chose.
- **The browser app is local and locked down:** it listens on 127.0.0.1 only, needs the one-time link,
  and rejects requests from other websites.

Known limit: between the final check and the removal there is a tiny window in which another program
could change the file. This is true of every file tool.

---

## 8. Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `REFUSED … no Recycle Bin/Trash is available for this location` | The drive has no Recycle Bin (network share, USB stick) or, on Linux, no writable `.Trash-$UID`. Move the files to an internal drive first, or use `--action delete --apply --yes-permanently-delete` if you really mean permanent deletion. |
| `REFUSED … has been modified since the scan` | The file changed after scanning. Re-scan and review again. |
| `path is too long for the Windows Recycle Bin` | The Windows Recycle Bin API can't handle paths over 260 characters. Shorten the folder path, or delete permanently. |
| macOS: `macOS blocked access to Finder` | Allow your terminal app under System Settings → Privacy & Security → Automation → Finder, then retry. |
| Browser shows "This sign-in link has already been used" | Press Enter in the terminal running `twinsweep ui` and open the new link. |
| Scan is slow on a spinning disk or NAS | Use `--workers 1` or `2`, `--min-size 1M`, or `--fast`. |
| Duplicates inside a folder that's itself a duplicate weren't listed | Twinsweep hides file groups fully inside duplicate folders. Clean the folder group, then re-scan. |
