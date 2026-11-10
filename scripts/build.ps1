# GoShare release build script.
#
# Usage:  powershell -File scripts/build.ps1 [-Version 0.6.0]
#
# Output: dist/goshare-v<Version>-windows-amd64.exe
#   (single file, no console window, icon + version info + PerMonitorV2 DPI manifest)
#
# NOTE: keep this file pure ASCII - Windows PowerShell 5.1 reads BOM-less
# .ps1 files as ANSI, and any non-ASCII byte sequence can break the parser.
#
# Generated resources (cmd/goshare/rsrc_*.syso) are NOT committed; they are
# regenerated on every build. Icon PNGs (winres/icons/) are committed;
# step 1 just keeps them in sync with internal/tray/icon.go.

param([string]$Version = "0.6.0")

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

# Mainland-friendly Go module settings (proxy.golang.org is unreachable here;
# same reason as PLAN constraint 7 for screencapture).
$env:GOPROXY = 'https://goproxy.cn,direct'
$env:GOSUMDB = 'sum.golang.google.cn'
$env:CGO_ENABLED = '0'

Write-Host "== 1/4 Generate icons (winres/icons) ==" -ForegroundColor Cyan
go run ./cmd/icon -out winres/icons
if ($LASTEXITCODE) { exit $LASTEXITCODE }

Write-Host "== 2/4 Embed Windows resources (icon/version/manifest) ==" -ForegroundColor Cyan
# P4-4: the version must come from ONE place ($Version).
# winres.json used to hard-code 0.7.0, so the exe's FileVersion silently drifted
# from the file name / main.version. Rewrite a temp copy with the real version.
$wj = Get-Content winres/winres.json -Raw
$wj = $wj -replace '"file_version": "[0-9.]+"', ('"file_version": "' + $Version + '.0"')
$wj = $wj -replace '"product_version": "[0-9.]+"', ('"product_version": "' + $Version + '.0"')
$wj = $wj -replace '"FileVersion": "[0-9.]+"', ('"FileVersion": "' + $Version + '"')
$wj = $wj -replace '"ProductVersion": "[0-9.]+"', ('"ProductVersion": "' + $Version + '"')
[IO.File]::WriteAllText("$PWD/winres/winres.generated.json", $wj)
go run github.com/tc-hib/go-winres@latest make --in winres/winres.generated.json --out cmd/goshare/rsrc
Remove-Item winres/winres.generated.json -ErrorAction SilentlyContinue
if ($LASTEXITCODE) { exit $LASTEXITCODE }

Write-Host "== 3/4 Build single-file exe ==" -ForegroundColor Cyan
New-Item -ItemType Directory -Force dist | Out-Null
$exe = "dist/goshare-v$Version-windows-amd64.exe"
# -trimpath is REQUIRED for anything you hand to other people: without it the
# binary embeds absolute build paths, which leak the build machine's user
# profile (e.g. C:\Users\<name>\go\pkg\mod\...) to anyone who runs `strings`.
go build -trimpath -ldflags "-H windowsgui -s -w -X main.version=$Version" -o $exe ./cmd/goshare
if ($LASTEXITCODE) { exit $LASTEXITCODE }

Write-Host "== 4/4 Verify ==" -ForegroundColor Cyan
$f = Get-Item $exe
$vi = $f.VersionInfo
Write-Host ("Output : {0}  ({1:N1} MB)" -f $f.FullName, ($f.Length / 1MB))
Write-Host ("Version: {0} - {1}" -f $vi.FileVersion, $vi.FileDescription)
if ($vi.FileVersion -notlike "$Version*") {
    Write-Warning "exe FileVersion ($($vi.FileVersion)) does not match -Version ($Version) - check fixed.file_version in winres/winres.json"
}
Write-Host "Done." -ForegroundColor Green
