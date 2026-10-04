# Installs Inno Setup 7 for the current user into -Dir, without admin, and prints the path of its ISCC.exe. GitHub's Windows runners no longer come with it, so the release and CI workflows fetch the pinned version here.
# The download is the official release on github.com/jrsoftware/issrc, checked against the pinned SHA256 and its Authenticode signature (Inno Setup's builds are signed by Pyrsys B.V.) before it runs.
param(
	[Parameter(Mandatory = $true)][string]$Dir
)
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$url = 'https://github.com/jrsoftware/issrc/releases/download/is-7_1_0/innosetup-7.1.0-x64.exe'
$sha256 = '0362a383ed217d4c4239b5933866dd96d3eb2102737da92f80f6057a4b40df2f'

$iscc = Join-Path $Dir 'ISCC.exe'
if (Test-Path $iscc) {
	$iscc
	return
}

$setup = Join-Path ([IO.Path]::GetTempPath()) 'innosetup-7.1.0-x64.exe'
Invoke-WebRequest -UseBasicParsing -Uri $url -OutFile $setup
$got = (Get-FileHash -Algorithm SHA256 -Path $setup).Hash.ToLower()
if ($got -ne $sha256) { throw "Inno Setup download has SHA256 $got, expected $sha256" }
$sig = Get-AuthenticodeSignature -FilePath $setup
if ($sig.Status -ne 'Valid' -or $sig.SignerCertificate.Subject -notmatch '(^|, )O=Pyrsys B\.V\.(,|$)') { throw "Inno Setup download is not signed by Pyrsys B.V. ($($sig.Status), $($sig.SignerCertificate.Subject))" }

# /TASKS="" leaves out the .iss file association and every other optional task; /NOICONS adds no Start menu entries.
$p = Start-Process -FilePath $setup -ArgumentList '/VERYSILENT', '/SUPPRESSMSGBOXES', '/NORESTART', '/CURRENTUSER', '/NOICONS', '/TASKS=""', "/DIR=$Dir" -PassThru
# Holding the process handle from the start is what keeps ExitCode readable once it has exited; Windows PowerShell otherwise reports it empty.
$null = $p.Handle
$p.WaitForExit()
Remove-Item -Force $setup
if ($p.ExitCode -ne 0) { throw "Inno Setup's installer exited with code $($p.ExitCode)" }
if (-not (Test-Path $iscc)) { throw "Inno Setup installed, but $iscc is not there" }
$iscc
