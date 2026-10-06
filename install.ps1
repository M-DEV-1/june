# Downloads the latest June installer for Windows, checks it against the release's SHA256SUMS, and runs it: the terminal alternative to downloading June-Setup-x64.exe from the release page.
#   irm https://raw.githubusercontent.com/M-DEV-1/june/main/install.ps1 | iex
# The installer's one page opens as usual. To install with no page at all (start at sign-in and the daily update check on, June opens when done), set $env:JUNE_SILENT = '1' first. Run it from a normal PowerShell, not an elevated one.
# Everything runs inside one script block: iex runs this in the caller's own session, where a changed $ErrorActionPreference would outlive the script and an exit would close the window.
& {
	$ErrorActionPreference = 'Stop'
	# Invoke-WebRequest's progress bar slows a Windows PowerShell 5.1 download many times over.
	$ProgressPreference = 'SilentlyContinue'
	$repo = 'M-DEV-1/june'
	$base = "https://github.com/$repo/releases/latest/download"
	$name = 'June-Setup-x64.exe'

	if (-not [Environment]::Is64BitOperatingSystem) { throw 'June needs 64-bit Windows.' }
	# Setup started from an elevated PowerShell is elevated too, and so is the June it opens, with every program June starts: the window, its PowerShell helpers and the AI command lines, which could then drive windows Windows otherwise shields from normal programs. June installs per user and never needs admin, so an elevated window is turned away while the same user has a normal one to use. Not when there is none: with UAC off, or as the built-in Administrator, whose token Windows does not split by default, every program runs elevated anyway.
	$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
	if (([Security.Principal.WindowsPrincipal]$identity).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
		$policy = Get-ItemProperty -Path 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System' -ErrorAction SilentlyContinue
		$uacOn = -not ($policy -and $policy.EnableLUA -eq 0)
		$splitToken = -not ($identity.User.Value -match '-500$' -and -not ($policy -and $policy.FilterAdministratorToken -eq 1))
		if ($uacOn -and $splitToken) {
			throw "This PowerShell is running as administrator. June installs for your Windows account only and needs no admin rights, and installed from here it would keep running with them. Open PowerShell normally (not with 'Run as administrator') and run the command again."
		}
	}
	# Windows PowerShell 5.1 can still default to TLS 1.0, which GitHub refuses.
	[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

	$tmp = Join-Path ([IO.Path]::GetTempPath()) ('june-install-' + [guid]::NewGuid().ToString('N'))
	New-Item -ItemType Directory -Path $tmp | Out-Null
	try {
		$installer = Join-Path $tmp $name
		Write-Host "downloading $name..."
		try {
			Invoke-WebRequest -UseBasicParsing -Uri "$base/$name" -OutFile $installer
		} catch {
			# GitHub answers 404 when the latest published release has no Windows installer, or there is no published release at all: drafts and pre-releases never count as latest.
			$response = $_.Exception.Response
			if ($response -and [int]$response.StatusCode -eq 404) {
				throw "No full Windows release of June is out yet. A release candidate may be: download June-Setup-x64.exe from https://github.com/$repo/releases"
			}
			throw
		}
		# Saved to a file rather than read from .Content, which Windows PowerShell returns as bytes for the octet-stream type GitHub serves.
		$sums = Join-Path $tmp 'SHA256SUMS'
		Invoke-WebRequest -UseBasicParsing -Uri "$base/SHA256SUMS" -OutFile $sums
		$want = Get-Content $sums | ForEach-Object {
			$fields = -split $_
			if ($fields.Count -eq 2 -and $fields[1].TrimStart('*') -eq $name) { $fields[0].ToLower() }
		} | Select-Object -First 1
		if (-not $want) { throw "the release's SHA256SUMS has no line for $name" }
		$got = (Get-FileHash -Algorithm SHA256 -Path $installer).Hash.ToLower()
		if ($got -ne $want) { throw "$name does not match the release's SHA256SUMS (expected $want, got $got); nothing was installed" }
		Write-Host "checksum ok ($got)"

		$start = @{ FilePath = $installer; PassThru = $true }
		if ($env:JUNE_SILENT -eq '1') { $start.ArgumentList = '/VERYSILENT', '/SUPPRESSMSGBOXES', '/NORESTART', '/RELAUNCH' }
		# WaitForExit rather than Start-Process -Wait, which also waits for every process the installer starts, and June keeps running after it opens.
		$setup = Start-Process @start
		# Holding the process handle from the start is what keeps ExitCode readable once it has exited; Windows PowerShell otherwise reports it empty.
		$null = $setup.Handle
		$setup.WaitForExit()
		# Inno Setup's code for the user closing the wizard before it installed anything.
		if ($setup.ExitCode -eq 2) {
			Write-Host 'Setup was cancelled; nothing was installed.'
			return
		}
		if ($setup.ExitCode -ne 0) { throw "June's installer exited with code $($setup.ExitCode)" }
		Write-Host 'June is installed. Open it from the Start menu any time.'
	} finally {
		Remove-Item -Recurse -Force -Path $tmp -ErrorAction SilentlyContinue
	}
}
