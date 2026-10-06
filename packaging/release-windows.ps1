# Builds June for Windows and packages it: the installer June-Setup-x64.exe and the portable june-windows-x86_64.zip. The Windows counterpart of packaging/release.sh; it installs nothing on this machine.
# The package holds three programs: june.exe, the console build that the terminal commands (june, june doctor) need; junew.exe, the same program built without a console, which the Start menu shortcut and the login entry run so no console window opens; and june-window.exe, the desktop window. Beside them, crt\ holds the Visual C++ and OpenMP runtimes the downloadable local models need (see windows\fetch-prereqs.ps1).
# Two stages, because a release signs the programs between them (.github/workflows/release.yml): build compiles into <Out>\exes and fetches the prerequisites into <Out>\prereqs; package turns a folder of programs (signed or not, -Exes) into the zip and the installer. With no -Stage it does both.
# -SignUninstaller (build stage, for a release that will be signed) also has Inno Setup write out its own Setup program, as <Out>\exes\uninst-*.exe, to be signed along with June's; given that signed file back in -Exes, the package stage builds an installer whose Setup and uninstaller carry the signature too (see SignedUninstallerDir in windows\june.iss).
# Needs Go, Rust with the MSVC toolchain, Node with pnpm, and for the installer Inno Setup 7 (ISCC.exe on PATH, in its default per-user or per-machine folder, or given with -Iscc). Run from any directory:
#   powershell -ExecutionPolicy Bypass -File packaging\release-windows.ps1
param(
	[ValidateSet('all', 'build', 'package')][string]$Stage = 'all',
	[string]$Version = $env:VERSION,
	[string]$Out = '',
	[string]$Exes = '',
	[string]$Iscc = '',
	[switch]$SignUninstaller
)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
# Relative -Out and -Exes mean relative to where the script was started, and ISCC would otherwise read relative paths against june.iss's own folder, so both become absolute before anything moves.
if (-not $Out) { $Out = Join-Path $root 'dist\windows' }
$Out = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($Out)
if (-not $Exes) { $Exes = Join-Path $Out 'exes' }
$Exes = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($Exes)
$prereqs = Join-Path $Out 'prereqs'
$uninstallerDir = Join-Path $Out 'uninstaller'
$iss = Join-Path $PSScriptRoot 'windows\june.iss'
$programs = 'june.exe', 'junew.exe', 'june-window.exe'
Set-Location $root

# Every native command is checked by hand, because $ErrorActionPreference does not stop on a non-zero exit code.
function Invoke-Checked([string]$what, [scriptblock]$run) {
	& $run
	if ($LASTEXITCODE -ne 0) { throw "$what failed with exit code $LASTEXITCODE" }
}

function Find-Iscc {
	if ($Iscc) { return $Iscc }
	$candidates = @(
		(Get-Command ISCC.exe -ErrorAction SilentlyContinue | Select-Object -ExpandProperty Source),
		(Join-Path $env:LOCALAPPDATA 'Programs\Inno Setup 7\ISCC.exe'),
		(Join-Path ${env:ProgramFiles} 'Inno Setup 7\ISCC.exe'),
		(Join-Path ${env:ProgramFiles(x86)} 'Inno Setup 7\ISCC.exe')
	)
	return $candidates | Where-Object { $_ -and (Test-Path $_) } | Select-Object -First 1
}

# The DLLs a PE file names in its import table, read straight from the file, since dumpbin exists only inside a Visual Studio prompt.
function Get-PeImports([string]$path) {
	$b = [IO.File]::ReadAllBytes($path)
	$pe = [BitConverter]::ToInt32($b, 0x3C)
	$sectionCount = [BitConverter]::ToUInt16($b, $pe + 6)
	$optional = $pe + 24
	$sections = $optional + [BitConverter]::ToUInt16($b, $pe + 20)
	# The import table is data directory 1, whose place depends on whether the optional header is PE32+ (0x20b) or PE32.
	$directories = $optional + $(if ([BitConverter]::ToUInt16($b, $optional) -eq 0x20b) { 112 } else { 96 })
	$toOffset = {
		param([uint32]$rva)
		for ($i = 0; $i -lt $sectionCount; $i++) {
			$s = $sections + 40 * $i
			$va = [BitConverter]::ToUInt32($b, $s + 12)
			$size = [Math]::Max([BitConverter]::ToUInt32($b, $s + 8), [BitConverter]::ToUInt32($b, $s + 16))
			if ($rva -ge $va -and $rva -lt $va + $size) { return [int]($rva - $va + [BitConverter]::ToUInt32($b, $s + 20)) }
		}
		throw "$path has no section holding RVA $rva"
	}
	$importRva = [BitConverter]::ToUInt32($b, $directories + 8)
	if ($importRva -eq 0) { return }
	for ($d = & $toOffset $importRva; ; $d += 20) {
		$nameRva = [BitConverter]::ToUInt32($b, $d + 12)
		if ($nameRva -eq 0) { break }
		$at = & $toOffset $nameRva
		[Text.Encoding]::ASCII.GetString($b, $at, [Array]::IndexOf($b, [byte]0, $at) - $at)
	}
}

$treeVersion = (Get-Content app\src-tauri\tauri.conf.json -Raw | ConvertFrom-Json).version
if (-not $treeVersion) { throw 'could not read a version from app\src-tauri\tauri.conf.json' }
if (-not $Version) { $Version = $treeVersion }
# The window takes its version from tauri.conf.json at compile time, so building any other number would ship programs that disagree about which June they are.
if ($Version -ne $treeVersion) { throw "VERSION is $Version but tauri.conf.json says $treeVersion; run packaging/bump-version.sh $Version first" }
if ($Version -notmatch '^(\d+)\.(\d+)\.(\d+)(-[0-9A-Za-z.-]+)?$') { throw "version '$Version' is not X.Y.Z or X.Y.Z-prerelease" }
# Windows file metadata takes four numbers and no prerelease text; the text version still carries it.
$numeric = "$($Matches[1]).$($Matches[2]).$($Matches[3]).0"

New-Item -ItemType Directory -Force $Out | Out-Null

if ($Stage -ne 'package') {
	Write-Host "building June $Version into $Exes"
	if (Test-Path $Exes) { Remove-Item -Recurse -Force $Exes }
	New-Item -ItemType Directory -Force $Exes | Out-Null

	if ($SignUninstaller) {
		# The first of Inno's two compiles for a signed uninstaller (see SignedUninstallerDir in windows\june.iss). It reads only [Setup] before it stops, so it takes seconds and needs none of June's files; first, so a missing Inno fails the build before the long part. The second compile, in the package stage, must see the same version and the same Inno Setup to find the file again.
		$isccPath = Find-Iscc
		if (-not $isccPath) { throw 'Inno Setup 7 (ISCC.exe) not found; pass -Iscc' }
		if (Test-Path $uninstallerDir) { Remove-Item -Recurse -Force $uninstallerDir }
		New-Item -ItemType Directory -Force $uninstallerDir | Out-Null
		Write-Host "writing out Inno Setup's program for signing; ISCC ends this step with an error asking for that file to be signed, which is expected"
		& $isccPath /Q "/DAppVersion=$Version" "/DNumericVersion=$numeric" "/DSignedUninstallerDir=$uninstallerDir" "/O$Out" $iss
		$unsigned = @(Get-ChildItem $uninstallerDir -Filter 'uninst-*.e*')
		if ($LASTEXITCODE -ne 2 -or $unsigned.Count -ne 1) { throw "ISCC exited $LASTEXITCODE and wrote $($unsigned.Count) uninstaller files; expected exit code 2 and one file" }
		# A GitHub pwsh step ends with the last native exit code, which would otherwise be this expected 2 if nothing ran after it.
		$global:LASTEXITCODE = 0
		# Signing services take .exe files; the package stage drops the extension again to give Inno back its own file name.
		Copy-Item $unsigned[0].FullName (Join-Path $Exes "$($unsigned[0].Name).exe")
	}

	Write-Host 'building the daemon...'
	# go-winres gives both Go programs June's icon and the product name and version Windows shows under Properties, which SignPath also requires of every file it signs. It writes a .syso into the main package, where go build picks it up; it is removed afterwards so it never lands in a commit or in a later plain go build. No manifest: the tracker sets its own DPI awareness at run time (internal/tracker/dpi_windows.go), and a manifest's would win.
	$syso = Join-Path $root 'rsrc_windows_amd64.syso'
	try {
		Invoke-Checked 'go-winres' { go run github.com/tc-hib/go-winres@v0.3.3 simply --arch amd64 --out rsrc --manifest none --icon app\src-tauri\icons\icon.ico --product-name June --file-description June --product-version $Version --file-version $numeric }
		$env:CGO_ENABLED = '0'
		$ldflags = "-s -w -X june/internal/config.Version=$Version"
		Invoke-Checked 'go build june.exe' { go build -trimpath -ldflags $ldflags -o (Join-Path $Exes 'june.exe') . }
		Invoke-Checked 'go build junew.exe' { go build -trimpath -ldflags "$ldflags -H windowsgui" -o (Join-Path $Exes 'junew.exe') . }
	} finally {
		Remove-Item -Force -ErrorAction SilentlyContinue $syso
	}

	Write-Host 'fetching the prerequisites...'
	if (Test-Path $prereqs) { Remove-Item -Recurse -Force $prereqs }
	Invoke-Checked 'fetch-prereqs' { powershell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $PSScriptRoot 'windows\fetch-prereqs.ps1') -Out $prereqs }

	Write-Host 'building the desktop window...'
	Push-Location app
	try {
		if (-not (Test-Path node_modules)) { Invoke-Checked 'pnpm install' { pnpm install --frozen-lockfile } }
		Invoke-Checked 'pnpm build' { pnpm build }
	} finally { Pop-Location }
	Push-Location app\src-tauri
	try {
		# Links the C runtime into june-window.exe, so a machine without the Visual C++ redistributable can still start it. tauri-build only does this when the variable is set, and the Tauri CLI is what normally sets it.
		$env:STATIC_VCRUNTIME = 'true'
		# tauri-build does not tell Cargo that it reads the variable, so a release folder left by a plain cargo build would keep its dynamically linked june-window.exe. Cleaning the one crate makes its build script run again; the dependencies stay built.
		Invoke-Checked 'cargo clean' { cargo clean --release -p june }
		Invoke-Checked 'cargo build' { cargo build --release --locked }
	} finally { Pop-Location }
	$window = Join-Path $Exes 'june-window.exe'
	Copy-Item app\src-tauri\target\release\june.exe $window
	$crt = @(Get-PeImports $window | Where-Object { $_ -match '^(vcruntime|msvcp)\d' })
	if ($crt) { throw "june-window.exe imports $($crt -join ', '), so it would not start on a PC without the Visual C++ redistributable; the C runtime was not linked in" }
}

if ($Stage -ne 'build') {
	foreach ($name in $programs) {
		if (-not (Test-Path (Join-Path $Exes $name))) { throw "$Exes has no $name" }
	}
	foreach ($name in 'crt\msvcp140.dll', 'crt\vcruntime140.dll', 'crt\vcruntime140_1.dll', 'crt\vcomp140.dll', 'MicrosoftEdgeWebview2Setup.exe') {
		if (-not (Test-Path (Join-Path $prereqs $name))) { throw "$prereqs has no $name; run the build stage first" }
	}

	# The name carries no version, so releases/latest/download/<name> is a link that never changes.
	Write-Host 'packing the portable archive...'
	$archive = Join-Path $Out 'june-windows-x86_64.zip'
	if (Test-Path $archive) { Remove-Item -Force $archive }
	# Entry by entry with names spelled out: Compress-Archive and ZipFile.CreateFromDirectory under Windows PowerShell 5.1 both write names with backslashes, which unzip tools other than Explorer take as part of the file name.
	Add-Type -AssemblyName System.IO.Compression, System.IO.Compression.FileSystem
	# The three programs by name: -Exes can also hold Inno's uninstaller, which must not reach the zip, as an unins*.exe beside June is what tells June's updater it was installed.
	$entries = @($programs | ForEach-Object { [pscustomobject]@{ Path = (Join-Path $Exes $_); Name = "june-$Version/$_" } })
	$entries += @(Get-ChildItem (Join-Path $prereqs 'crt\*.dll') | ForEach-Object { [pscustomobject]@{ Path = $_.FullName; Name = "june-$Version/crt/$($_.Name)" } })
	$zip = [IO.Compression.ZipFile]::Open($archive, [IO.Compression.ZipArchiveMode]::Create)
	try {
		foreach ($entry in $entries) {
			[void][IO.Compression.ZipFileExtensions]::CreateEntryFromFile($zip, $entry.Path, $entry.Name, [IO.Compression.CompressionLevel]::Optimal)
		}
	} finally { $zip.Dispose() }

	$isccPath = Find-Iscc
	if ($isccPath) {
		Write-Host "building the installer with $isccPath..."
		$installer = Join-Path $Out 'June-Setup-x64.exe'
		if (Test-Path $installer) { Remove-Item -Force $installer }
		$defines = @("/DAppVersion=$Version", "/DNumericVersion=$numeric", "/DExeDir=$Exes", "/DPrereqDir=$prereqs")
		$signedUninstaller = @(Get-ChildItem $Exes -Filter 'uninst-*.exe')
		if ($signedUninstaller.Count -gt 1) { throw "$Exes holds more than one uninst-*.exe" }
		if ($signedUninstaller) {
			# Back under the name the build stage's compile gave it, where this compile looks for it; ISCC stops with an error if it is not signed or not the program it would build.
			Write-Host "using the signed Setup program $($signedUninstaller[0].Name)"
			New-Item -ItemType Directory -Force $uninstallerDir | Out-Null
			Copy-Item -Force $signedUninstaller[0].FullName (Join-Path $uninstallerDir $signedUninstaller[0].BaseName)
			$defines += "/DSignedUninstallerDir=$uninstallerDir"
		}
		Invoke-Checked 'ISCC' { & $isccPath /Q @defines "/O$Out" $iss }
	} elseif ($Stage -eq 'package') {
		throw 'Inno Setup 7 (ISCC.exe) not found; pass -Iscc'
	} else {
		Write-Warning 'Inno Setup 7 (ISCC.exe) not found, so no installer was built; the portable zip was'
	}

	Get-ChildItem $Out -File | ForEach-Object { '{0}  {1}  {2,10}' -f (Get-FileHash -Algorithm SHA256 $_.FullName).Hash.ToLower(), $_.Name, $_.Length }
}
