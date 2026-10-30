<#
.SYNOPSIS
  Publishes a Twinsweep version's binaries as a GitHub Release.

.DESCRIPTION
  Run AFTER a version has been committed, tagged (with the security-review
  approval line) and pushed - see docs/development.md, "Releasing a version".
  Runs under PowerShell 7 (pwsh) on Windows, macOS or Linux. Needs git, Go
  and the GitHub CLI (`gh`, signed in with `gh auth login`).

  What it does, in order:
    1. Refuses unless the tag vX.Y.Z exists as an annotated tag whose message
       contains "Security review: approved" - the security gate is enforced
       here, not just remembered - and the tag has been pushed to origin.
    2. Checks out that exact tag into a temporary, separate folder (a "git
       worktree": a second checkout sharing this repo's history), so the
       binaries are built from the tagged commit, never from whatever happens
       to be in your working folder.
    3. Runs that checkout's own scripts/build.ps1 (6 OS/CPU targets plus
       SHA256SUMS.txt), then confirms the binary for this machine reports the
       expected version.
    4. Creates the GitHub Release "Twinsweep X.Y.Z" for the tag, with that
       version's CHANGELOG.md section as the release notes, and uploads the
       binaries and checksums.
    5. Removes the temporary checkout.

  Safety: it never creates or moves tags, never pushes commits, and refuses
  to touch a release that already exists unless -Replace is given (which
  re-uploads the files, overwriting same-named ones). -DryRun does steps 1-3
  and shows what would be published, without contacting GitHub.

  Only writes inside the temporary checkout (deleted at the end) and, for a
  real run, to the GitHub Release.

.PARAMETER Version
  Version to release, e.g. 0.2.0. Default: the version in
  internal/version/version.go of the current checkout.

.PARAMETER DryRun
  Build and verify only; print what would be uploaded.

.PARAMETER Replace
  Allow re-uploading files to an existing release for this tag.

.EXAMPLE
  pwsh ./scripts/release.ps1 -DryRun
  pwsh ./scripts/release.ps1
  pwsh ./scripts/release.ps1 -Version 0.2.0
#>
[CmdletBinding()]
param(
  [string]$Version,
  [switch]$DryRun,
  [switch]$Replace
)
$ErrorActionPreference = 'Stop'
Set-Location (Join-Path $PSScriptRoot '..')

<#
  Fail prints a message in red and stops the script with exit code 1.
#>
function Fail([string]$Message) {
  Write-Host "release: $Message" -ForegroundColor Red
  exit 1
}

# ---- Which version ----
if (-not $Version) {
  $Version = (Select-String -Path internal/version/version.go -Pattern 'Version = "([^"]+)"').Matches[0].Groups[1].Value
}
# Regex: three dot-separated numbers, e.g. 0.2.0 (no "v" prefix).
if ($Version -notmatch '^\d+\.\d+\.\d+$') { Fail "version '$Version' is not in X.Y.Z form" }
$tag = "v$Version"
Write-Host "Releasing Twinsweep $Version ($tag)" -ForegroundColor Cyan

# ---- 1. Gate checks ----
# "<tag>^{tag}" resolves only for an ANNOTATED tag (lightweight tags carry no
# message, so they can't carry the approval line).
git rev-parse --verify --quiet "$tag^{tag}" *> $null
if ($LASTEXITCODE -ne 0) { Fail "$tag is not an annotated tag in this repo. Tag the approved commit first (docs/development.md)." }
$tagMessage = git tag -l --format='%(contents)' $tag | Out-String
if ($tagMessage -notmatch 'Security review: approved') {
  Fail "$tag's message has no 'Security review: approved' line - the security gate has not been recorded for this version."
}
$remote = git ls-remote --tags origin "refs/tags/$tag"
if (-not $remote) { Fail "$tag has not been pushed to origin. Run: git push origin $tag" }
$commit = git rev-list -n 1 $tag
Write-Host "  tag OK: $tag -> $($commit.Substring(0,7)), security approval recorded, pushed"

if (-not $DryRun) {
  gh auth status *> $null
  if ($LASTEXITCODE -ne 0) { Fail "GitHub CLI is not signed in. Run: gh auth login" }
  gh release view $tag *> $null
  $exists = $LASTEXITCODE -eq 0
  if ($exists -and -not $Replace) { Fail "a GitHub Release for $tag already exists. Use -Replace to re-upload its files." }
}

# ---- 2. Temporary checkout of the tag ----
$wt = Join-Path ([IO.Path]::GetTempPath()) "twinsweep-release-$tag-$PID"
git worktree add --detach --quiet $wt $tag
if ($LASTEXITCODE -ne 0) { Fail "could not check out $tag into $wt" }
try {
  # ---- 3. Build and verify ----
  pwsh -NoProfile -File (Join-Path $wt 'scripts/build.ps1')
  if ($LASTEXITCODE -ne 0) { Fail 'build failed' }
  $dist = Join-Path $wt 'dist'
  $files = Get-ChildItem $dist -File | Sort-Object Name
  if ($files.Count -lt 7) { Fail "expected 6 binaries + SHA256SUMS.txt in dist/, found $($files.Count) files" }

  # Run the binary built for this machine and check it reports the version.
  $os = if ($IsWindows) { 'windows' } elseif ($IsMacOS) { 'darwin' } else { 'linux' }
  $arch = if ([Runtime.InteropServices.RuntimeInformation]::OSArchitecture -eq 'Arm64') { 'arm64' } else { 'amd64' }
  $ext = if ($IsWindows) { '.exe' } else { '' }
  $self = Join-Path $dist "twinsweep-$Version-$os-$arch$ext"
  $reported = & $self version
  if ($reported -notmatch [regex]::Escape("twinsweep $Version ")) { Fail "built binary reports '$reported', expected version $Version" }
  Write-Host "  build OK: $reported"

  # Release notes = this version's CHANGELOG.md section (from the tagged
  # commit), i.e. everything after "## [X.Y.Z]" up to the next "## [".
  $changelog = Get-Content (Join-Path $wt 'CHANGELOG.md') -Raw
  $pattern = '(?ms)^## \[' + [regex]::Escape($Version) + '\][^\n]*\n(.*?)(?=^## \[|\z)'
  $m = [regex]::Match($changelog, $pattern)
  if (-not $m.Success) { Fail "CHANGELOG.md at $tag has no section for $Version" }
  $notes = $m.Groups[1].Value.Trim() + "`n`n**Downloads:** pick the file for your OS/CPU below; verify it with ``SHA256SUMS.txt``. See the [user guide](https://github.com/jasonhaymond/Twinsweep/blob/$tag/docs/user-guide.md#1-install)."
  $notesFile = Join-Path $wt 'release-notes.md'
  Set-Content -Path $notesFile -Value $notes -Encoding utf8

  if ($DryRun) {
    Write-Host "`nDRY RUN - nothing published. Would upload to GitHub Release '$tag':" -ForegroundColor Yellow
    $files | ForEach-Object { '  {0,-42} {1,8:N1} MB' -f $_.Name, ($_.Length / 1MB) } | Write-Host
    Write-Host "`nRelease notes:`n$notes"
    exit 0
  }

  # ---- 4. Publish ----
  $paths = $files.FullName
  if ($exists) {
    gh release upload $tag @paths --clobber
  } else {
    # --verify-tag: refuse rather than let gh create a missing tag itself.
    gh release create $tag @paths --title "Twinsweep $Version" --notes-file $notesFile --verify-tag
  }
  if ($LASTEXITCODE -ne 0) { Fail 'GitHub upload failed' }
  $url = gh release view $tag --json url -q .url
  Write-Host "`nPublished: $url" -ForegroundColor Green
}
finally {
  # ---- 5. Clean up the temporary checkout ----
  git worktree remove --force $wt *> $null
  git worktree prune *> $null # forget the checkout even if its folder was already deleted
}
