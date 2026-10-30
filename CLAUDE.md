# Twinsweep — project notes for Claude

Cross-platform duplicate file/folder scanner and cleanup tool (Go, single binary). Global standards in
`~/.claude/CLAUDE.md` apply; this file narrows them for this project.

- **Not a server app:** no database, deploy script, staging, health endpoint, backups, or admin UI apply.
  `serve` is a short-lived localhost review page, not a deployed service.
- **Safety-critical code:** `internal/clean` (the only remover), `internal/pathutil`, `internal/trash`.
  Any change there needs a matching test in the same change proving the data-loss case stays blocked.
- **Security gate:** `pwsh ./scripts/security-check.ps1`, plus the WSL Linux test pass in
  `docs/development.md` §3 (symlink tests skip on Windows).
- **UI changes:** screenshot `serve` at desktop and ~390 px, light and dark (puppeteer-core + Edge works;
  don't use `isMobile: true` — it reloads the page and wipes state).
- **Version:** `internal/version/version.go` is the single source; bump with CHANGELOG.md.
- **macOS trash** is written to Apple's documented approach but has not been run on a real Mac yet.
