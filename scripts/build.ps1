<#
.SYNOPSIS
  Builds Twinsweep release binaries for every supported OS/CPU combination.

.DESCRIPTION
  Cross-compiles cmd/twinsweep into dist/ for Windows, macOS and Linux on
  both x86-64 (amd64) and ARM (arm64), then writes dist/SHA256SUMS.txt so
  people downloading a binary can confirm it wasn't corrupted or tampered
  with. Go builds every target from any one machine; no other toolchain is
  needed. Runs under PowerShell 7 (pwsh) on Windows, macOS or Linux.

  Only touches the dist/ folder inside this repository.

  Build flags, in plain terms:
    CGO_ENABLED=0  no C compiler involved -> fully static, portable binaries
    -trimpath      strip this machine's folder paths from the binary
    -ldflags -s -w drop debug symbols (smaller file)

.EXAMPLE
  pwsh ./scripts/build.ps1
#>
$ErrorActionPreference = 'Stop'
Set-Location (Join-Path $PSScriptRoot '..')

# The version lives in one place: internal/version/version.go.
$version = (Select-String -Path internal/version/version.go -Pattern 'Version = "([^"]+)"').Matches[0].Groups[1].Value
Write-Host "Building Twinsweep $version"

$targets = @(
  @{ os = 'windows'; arch = 'amd64' }, @{ os = 'windows'; arch = 'arm64' },
  @{ os = 'darwin';  arch = 'amd64' }, @{ os = 'darwin';  arch = 'arm64' },
  @{ os = 'linux';   arch = 'amd64' }, @{ os = 'linux';   arch = 'arm64' }
)

# Start from an empty dist/ so stale binaries from an older version can't
# end up in a release by accident.
if (Test-Path dist) { Remove-Item -Recurse -Force dist }
New-Item -ItemType Directory dist | Out-Null

$env:CGO_ENABLED = '0'
foreach ($t in $targets) {
  $ext = if ($t.os -eq 'windows') { '.exe' } else { '' }
  $out = "dist/twinsweep-$version-$($t.os)-$($t.arch)$ext"
  $env:GOOS = $t.os
  $env:GOARCH = $t.arch
  go build -trimpath -ldflags '-s -w' -o $out ./cmd/twinsweep
  if ($LASTEXITCODE -ne 0) { throw "build failed for $($t.os)/$($t.arch)" }
  Write-Host "  $out"
}
Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED

# Checksums in the same format as `sha256sum` so `sha256sum -c` works on Linux/macOS.
# Written with Unix (LF) line endings on every OS: Set-Content would use
# Windows CRLF when run on Windows, and the stray "\r" then becomes part of
# each file name, so `sha256sum -c` reports "no file was verified".
$lines = Get-ChildItem dist -File | Where-Object Name -ne 'SHA256SUMS.txt' | Sort-Object Name | ForEach-Object {
  "{0}  {1}" -f (Get-FileHash $_.FullName -Algorithm SHA256).Hash.ToLower(), $_.Name
}
$sums = Join-Path (Resolve-Path dist) 'SHA256SUMS.txt'
[IO.File]::WriteAllText($sums, ($lines -join "`n") + "`n", [Text.Encoding]::ASCII)
Write-Host "Wrote dist/SHA256SUMS.txt"
