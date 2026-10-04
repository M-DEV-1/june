# Gathers the two things the Windows package ships but does not build: the Visual C++ runtime DLLs and Microsoft's WebView2 bootstrapper, into <Out>\crt\ and <Out>\MicrosoftEdgeWebview2Setup.exe.
# The CRT goes in crt\ beside June because the whisper.cpp and llama.cpp builds June downloads later import msvcp140.dll, vcruntime140.dll and vcruntime140_1.dll, and their ggml libraries also import vcomp140.dll, Microsoft's OpenMP runtime, which comes with the same redistributable and not with Windows. Installing the redistributable system-wide needs admin, which June's installer never asks for. June's own programs need none of it: Go links nothing, and the window links its CRT statically.
# Every file is checked to carry a valid Microsoft Authenticode signature before it is used, so a tampered download or a stray DLL fails the build instead of shipping.
# Usage: powershell -ExecutionPolicy Bypass -File packaging\windows\fetch-prereqs.ps1 -Out dist\windows\prereqs
param(
	[Parameter(Mandatory = $true)][string]$Out
)
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

# Each DLL with the redistributable folder it comes from: the C++ runtime from Microsoft.VC14x.CRT, OpenMP from Microsoft.VC14x.OpenMP.
$crtNames = [ordered]@{ 'msvcp140.dll' = 'CRT'; 'vcruntime140.dll' = 'CRT'; 'vcruntime140_1.dll' = 'CRT'; 'vcomp140.dll' = 'OpenMP' }
$webView2Url = 'https://go.microsoft.com/fwlink/p/?LinkId=2124703'

function Assert-MicrosoftSigned([string]$path) {
	$sig = Get-AuthenticodeSignature -FilePath $path
	if ($sig.Status -ne 'Valid') { throw "$path has no valid Authenticode signature ($($sig.Status))" }
	if ($sig.SignerCertificate.Subject -notmatch '(^|, )O=Microsoft Corporation(,|$)') { throw "$path is signed by '$($sig.SignerCertificate.Subject)', not Microsoft" }
}

# The x64 folder of the newest redistributable version of the newest Visual Studio with the C++ tools, which is where Microsoft puts the DLLs it allows to be redistributed beside an application, one folder per runtime. Only a version that has both runtimes counts, so all four DLLs come from the same one.
function Find-RedistX64 {
	$vswhere = Join-Path ${env:ProgramFiles(x86)} 'Microsoft Visual Studio\Installer\vswhere.exe'
	if (-not (Test-Path $vswhere)) { return $null }
	foreach ($vs in @(& $vswhere -latest -products * -property installationPath)) {
		if (-not $vs) { continue }
		$redist = Join-Path $vs 'VC\Redist\MSVC'
		if (-not (Test-Path $redist)) { continue }
		$x64 = Get-ChildItem $redist -Directory |
			Where-Object { $_.Name -match '^\d+(\.\d+)+$' } |
			Sort-Object { [version]$_.Name } -Descending |
			ForEach-Object { Join-Path $_.FullName 'x64' } |
			Where-Object { (Get-ChildItem $_ -Directory -Filter 'Microsoft.VC14*.CRT' -ErrorAction SilentlyContinue) -and (Get-ChildItem $_ -Directory -Filter 'Microsoft.VC14*.OpenMP' -ErrorAction SilentlyContinue) } |
			Select-Object -First 1
		if ($x64) { return $x64 }
	}
	return $null
}

New-Item -ItemType Directory -Force (Join-Path $Out 'crt') | Out-Null

$x64 = Find-RedistX64
if (-not $x64) {
	# A release must ship the redistributable copies, so CI stops here. A developer machine with only the runtime installed gets the System32 copies, which are the same Microsoft-signed files, with a warning.
	if ($env:GITHUB_ACTIONS -eq 'true') { throw 'no Visual Studio VC\Redist\MSVC\<version>\x64 folder with both Microsoft.VC14*.CRT and Microsoft.VC14*.OpenMP found via vswhere' }
	Write-Warning 'no Visual Studio redistributable folder found; using the copies in System32 (fine for a local build, not for a release)'
}
foreach ($name in $crtNames.Keys) {
	$dir = Join-Path $env:SystemRoot 'System32'
	if ($x64) { $dir = (Get-ChildItem $x64 -Directory -Filter "Microsoft.VC14*.$($crtNames[$name])" | Select-Object -First 1).FullName }
	$src = Join-Path $dir $name
	if (-not (Test-Path $src)) { throw "$src is missing" }
	Assert-MicrosoftSigned $src
	Copy-Item $src (Join-Path $Out "crt\$name") -Force
	Write-Host "$name from $dir"
}

$bootstrapper = Join-Path $Out 'MicrosoftEdgeWebview2Setup.exe'
Invoke-WebRequest -UseBasicParsing -Uri $webView2Url -OutFile $bootstrapper
Assert-MicrosoftSigned $bootstrapper
Write-Host "WebView2 bootstrapper $((Get-Item $bootstrapper).VersionInfo.FileVersion)"

Get-ChildItem $Out -Recurse -File | ForEach-Object { '{0,-40} {1,10}' -f $_.FullName.Substring((Resolve-Path $Out).Path.Length + 1), $_.Length }
