<#
.SYNOPSIS
  Runs Twinsweep's full security suite and writes a report.

.DESCRIPTION
  One command for every security check (see docs/development.md,
  "Security review gate"). Runs under PowerShell 7 (pwsh) on any OS.

  Checks, in order:
    1. gofmt + go vet       - formatting and Go's built-in bug checks
    2. staticcheck          - deeper static analysis (bugs, dead code)
    3. gosec                - Go security linter (SAST)
    4. semgrep              - OWASP/Go rulesets (SAST), if installed
    5. govulncheck          - known vulnerabilities in dependencies AND in
                              the Go standard library version used, limited
                              to code paths Twinsweep actually calls
    6. gitleaks             - secrets in the working tree and staged diff, if installed
    7. go test (Windows)    - every test, including the security tests
    8. go list -m -u        - outdated dependencies (informational)

  staticcheck/gosec/govulncheck are fetched at their latest release via
  `go run ...@latest`, so nothing needs installing first. semgrep and
  gitleaks are separate programs; if missing they are reported NOT RUN,
  never silently skipped or shown as passing.

  The Linux test pass (symlink and freedesktop Trash tests that Windows
  can't run without Developer Mode) is covered by `go test` when this
  script runs on Linux/macOS, or see docs/development.md for the WSL recipe.

  Output: a summary table on screen plus a full Markdown report in
  security-reports/ (gitignored - reports describe unfixed weaknesses).
  Exit code 0 only if every check that ran passed.

  Read-only apart from writing the report file.

.EXAMPLE
  pwsh ./scripts/security-check.ps1
#>
$ErrorActionPreference = 'Continue'
Set-Location (Join-Path $PSScriptRoot '..')

$version = (Select-String -Path internal/version/version.go -Pattern 'Version = "([^"]+)"').Matches[0].Groups[1].Value
$commit = (git rev-parse --short HEAD 2>$null)
if (-not $commit) { $commit = 'uncommitted' }
$dirty = if (git status --porcelain 2>$null) { ' (+ uncommitted changes)' } else { '' }
$stamp = Get-Date -Format 'yyyyMMdd-HHmmss'

# Each check records: name, status (PASS/FAIL/NOT RUN), a one-line summary,
# and its full output for the report.
$results = [System.Collections.Generic.List[object]]::new()

function Invoke-Check {
  <#
    Runs one check. $Script is a script block returning nothing; the check
    PASSES when the last native command exits 0, unless $Pass overrides it.
  #>
  param([string]$Name, [scriptblock]$Script, [scriptblock]$Pass)
  Write-Host "-> $Name" -ForegroundColor Cyan
  $out = & $Script 2>&1 | Out-String
  $code = $LASTEXITCODE
  $ok = if ($Pass) { & $Pass $out $code } else { $code -eq 0 }
  $status = if ($ok) { 'PASS' } else { 'FAIL' }
  $summary = ($out.Trim() -split "`n" | Where-Object { $_.Trim() } | Select-Object -Last 1)
  $results.Add([pscustomobject]@{ Check = $Name; Status = $status; Summary = "$summary".Trim(); Output = $out })
}

function Add-NotRun {
  param([string]$Name, [string]$Reason)
  Write-Host "-> $Name : NOT RUN ($Reason)" -ForegroundColor Yellow
  $results.Add([pscustomobject]@{ Check = $Name; Status = 'NOT RUN'; Summary = $Reason; Output = '' })
}

# 1. Formatting + vet. gofmt -l lists unformatted files; any output = fail.
Invoke-Check 'gofmt + go vet' { gofmt -l .; go vet ./... } { param($o, $c) $c -eq 0 -and -not ($o -match '\.go') }

# 2-3. Static analysis (latest releases).
Invoke-Check 'staticcheck' { go run honnef.co/go/tools/cmd/staticcheck@latest ./... }
# gosec: any unsuppressed finding fails the run. `-show-ignored` lists every
# `#nosec`-suppressed line (each carries a justification) in the report so
# suppressions stay visible - but it also makes gosec exit 1 when anything
# is suppressed, so pass/fail is decided by its "Issues : N" count instead.
Invoke-Check 'gosec (SAST)' { go run github.com/securego/gosec/v2/cmd/gosec@latest -fmt text -show-ignored ./... } {
  param($o, $c) $o -match 'Issues\s*:\s*0\b'
}

# 4. semgrep (optional external tool).
if (Get-Command semgrep -ErrorAction SilentlyContinue) {
  Invoke-Check 'semgrep (OWASP/Go)' { semgrep scan --config p/golang --config p/owasp-top-ten --error --quiet . }
} else { Add-NotRun 'semgrep (OWASP/Go)' 'semgrep is not installed (pip install semgrep)' }

# 5. Known vulnerabilities in deps + standard library.
Invoke-Check 'govulncheck (dependencies)' { go run golang.org/x/vuln/cmd/govulncheck@latest ./... }

# 6. Secret scanning (optional external tool).
if (Get-Command gitleaks -ErrorAction SilentlyContinue) {
  Invoke-Check 'gitleaks (working tree)' { gitleaks dir . --no-banner --redact }
  if (git rev-parse --git-dir 2>$null) {
    Invoke-Check 'gitleaks (staged diff)' { gitleaks git --pre-commit --staged --no-banner --redact . }
  } else { Add-NotRun 'gitleaks (staged diff)' 'not a git repository yet' }
} else {
  Add-NotRun 'gitleaks (working tree)' 'gitleaks is not installed (winget install gitleaks / brew install gitleaks)'
  Add-NotRun 'gitleaks (staged diff)' 'gitleaks is not installed'
}

# 7. All tests (each security test documents the attack it blocks).
$os = if ($IsWindows) { 'Windows' } elseif ($IsMacOS) { 'macOS' } else { 'Linux' }
Invoke-Check "go test ($os)" { go test -count=1 ./... }

# 8. Outdated dependencies: informational, never fails the run.
Write-Host '-> outdated dependencies' -ForegroundColor Cyan
$outdated = go list -m -u -f '{{if .Update}}{{.Path}} {{.Version}} -> {{.Update.Version}}{{end}}' all 2>&1 | Where-Object { $_ } | Out-String
$results.Add([pscustomobject]@{ Check = 'outdated deps (info)'; Status = 'PASS'; Summary = $(if ($outdated.Trim()) { 'updates available' } else { 'all current' }); Output = $outdated })

# ---- Report ----
$table = $results | ForEach-Object { "| $($_.Check) | **$($_.Status)** | $($_.Summary -replace '\|', '/') |" }
$md = @"
# Twinsweep security report

- Version: $version
- Commit: $commit$dirty
- Date: $(Get-Date -Format 'yyyy-MM-dd HH:mm:ss zzz')
- Host OS: $os

## Summary

| Check | Status | Summary |
|---|---|---|
$($table -join "`n")

## Findings / full output

$(($results | ForEach-Object { "### $($_.Check) - $($_.Status)`n`n``````text`n$($_.Output.Trim())`n```````n" }) -join "`n")

## Accepted risks

(none recorded)
"@
New-Item -ItemType Directory -Force security-reports | Out-Null
$file = "security-reports/v$version-$commit-$stamp.md"
Set-Content -Path $file -Value $md -Encoding utf8

Write-Host ''
$results | Format-Table Check, Status, Summary -AutoSize -Wrap | Out-String -Width 160 | Write-Host
Write-Host "Report: $file"
if ($results.Status -contains 'FAIL') { exit 1 }
exit 0
