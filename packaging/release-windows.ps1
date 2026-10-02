# Builds the Go daemon and the Tauri desktop window in release mode on Windows and stages a downloadable package under dist\june-<version>\, zipped to dist\june-windows-x86_64.zip. The Windows counterpart of packaging/release.sh; it installs nothing.
# The package holds three programs: june.exe, the console build that the terminal commands (june, june doctor) need; junew.exe, the same program built without a console, which the login entry runs so no console window opens at login; and june-window.exe, the desktop window.
# Needs Go, Rust with the MSVC toolchain, Node with pnpm, and the WebView2 runtime. Run from any directory: powershell -ExecutionPolicy Bypass -File packaging\release-windows.ps1
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

# Every native command is checked by hand, because $ErrorActionPreference does not stop on a non-zero exit code.
function Invoke-Checked([string]$what, [scriptblock]$run) {
	& $run
	if ($LASTEXITCODE -ne 0) { throw "$what failed with exit code $LASTEXITCODE" }
}

$version = (Get-Content app\src-tauri\tauri.conf.json -Raw | ConvertFrom-Json).version
if (-not $version) { throw 'could not read a version from app\src-tauri\tauri.conf.json' }
$dist = Join-Path $root "dist\june-$version"

Write-Host "staging into $dist"
if (Test-Path $dist) { Remove-Item -Recurse -Force $dist }
New-Item -ItemType Directory -Force $dist | Out-Null

Write-Host 'building the daemon...'
$env:CGO_ENABLED = '0'
Invoke-Checked 'go build june.exe' { go build -o "$dist\june.exe" . }
Invoke-Checked 'go build junew.exe' { go build -ldflags '-H windowsgui' -o "$dist\junew.exe" . }

Write-Host 'building the desktop window...'
Push-Location app
try {
	if (-not (Test-Path node_modules)) { Invoke-Checked 'pnpm install' { pnpm install --frozen-lockfile } }
	Invoke-Checked 'pnpm build' { pnpm build }
} finally { Pop-Location }
Push-Location app\src-tauri
try {
	Invoke-Checked 'cargo build' { cargo build --release }
} finally { Pop-Location }
Copy-Item app\src-tauri\target\release\june.exe "$dist\june-window.exe"

# The name carries no version, so releases/latest/download/<name> is a link that never changes.
Write-Host 'packing the archive...'
$archive = Join-Path $root 'dist\june-windows-x86_64.zip'
if (Test-Path $archive) { Remove-Item -Force $archive }
Compress-Archive -Path $dist -DestinationPath $archive
$hash = (Get-FileHash -Algorithm SHA256 $archive).Hash.ToLower()
Set-Content -Path (Join-Path $root 'dist\SHA256SUMS-windows') -Value "$hash  june-windows-x86_64.zip"

Write-Host "staged: $dist"
Write-Host "archive: $archive"
Get-ChildItem $dist | Format-Table Name, Length
