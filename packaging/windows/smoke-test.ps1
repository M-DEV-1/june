# Installs, upgrades and uninstalls June-Setup-x64.exe silently and fails on the first thing that is not as the installer promises: files and shortcut in place, start at sign-in and the update check set as chosen, a running June stopped and relaunched by an upgrade, june.exe --quit, data kept by a plain uninstall, a June running from another folder stopped by a first install, and data deleted by /PURGE.
# It writes the current user's Run key and Start menu, so it is for a throwaway machine such as a CI runner, never one whose June matters. June runs headless throughout, on its own port and data folder.
# Usage: pwsh packaging\windows\smoke-test.ps1 -Installer dist\windows\June-Setup-x64.exe
param(
	[Parameter(Mandatory = $true)][string]$Installer,
	[string]$Work = (Join-Path ([IO.Path]::GetTempPath()) 'june-smoke'),
	[string]$Port = '47231'
)
$ErrorActionPreference = 'Stop'
$Installer = (Resolve-Path $Installer).Path
$app = Join-Path $Work 'app'
$data = Join-Path $Work 'data'
# The installer passes its environment to everything it runs (june.exe --autostart, june.exe --quit, the relaunched June), so these two keep the whole test off the machine's real June.
$env:JUNE_DATA_DIR = $data
$env:JUNE_PORT = $Port
$runKey = 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Run'
$shortcut = Join-Path ([Environment]::GetFolderPath('Programs')) 'June.lnk'

function Step([string]$what) { Write-Host "== $what" }
function Assert([bool]$condition, [string]$what) {
	if (-not $condition) { throw "FAILED: $what" }
	Write-Host "ok: $what"
}
function Wait-Until([scriptblock]$condition, [int]$seconds) {
	$deadline = (Get-Date).AddSeconds($seconds)
	while ((Get-Date) -lt $deadline) {
		if (& $condition) { return $true }
		Start-Sleep -Milliseconds 250
	}
	return [bool](& $condition)
}
function Invoke-Program([string]$exe, [string[]]$arguments) {
	# Start-Process joins the arguments with spaces and quotes none of them, so a /NAME=value whose value has a space gets its quotes here.
	$quoted = $arguments | ForEach-Object { if ($_ -match '^(/\w+=)(.*\s.*)$') { "$($Matches[1])`"$($Matches[2])`"" } else { $_ } }
	$p = Start-Process -FilePath $exe -ArgumentList $quoted -PassThru
	# Holding the handle keeps ExitCode readable after the process has gone.
	$null = $p.Handle
	$p.WaitForExit()
	if ($p.ExitCode -ne 0) { throw "$exe $($arguments -join ' ') exited with code $($p.ExitCode)" }
}
function Test-Ping {
	try { return (Invoke-WebRequest -UseBasicParsing -TimeoutSec 2 -Uri "http://127.0.0.1:$Port/ping").StatusCode -eq 200 } catch { return $false }
}
function Get-InstalledJune {
	@(Get-Process june, junew, june-window -ErrorAction SilentlyContinue | Where-Object { $_.Path -and $_.Path.StartsWith("$app\", [StringComparison]::OrdinalIgnoreCase) })
}
function Get-RunValue { (Get-ItemProperty -Path $runKey -Name June -ErrorAction SilentlyContinue).June }
function Start-Daemon {
	Start-Process -FilePath (Join-Path $app 'junew.exe') -ArgumentList '--daemon' -WorkingDirectory $app | Out-Null
	Assert (Wait-Until { Test-Ping } 60) 'the daemon answers /ping'
}
# The uninstaller hands over to a copy of itself in %TEMP% and may return before that copy has finished, so its results are waited for rather than assumed.
function Invoke-Uninstall([string[]]$extra) {
	Invoke-Program (Join-Path $app 'unins000.exe') (@('/VERYSILENT', '/SUPPRESSMSGBOXES', '/NORESTART') + $extra)
	Assert (Wait-Until { -not (Test-Path (Join-Path $app 'unins000.exe')) } 120) 'the uninstaller finished'
}

if (Test-Path $Work) { Remove-Item -Recurse -Force $Work }
New-Item -ItemType Directory -Force $data | Out-Null
Set-Content -Path (Join-Path $data 'june-config.json') -Value '{"window": false}' -Encoding ascii

try {
	Step 'fresh install, silent, default tasks'
	Invoke-Program $Installer @('/VERYSILENT', '/SUPPRESSMSGBOXES', '/NORESTART', "/DIR=$app", "/LOG=$Work\install.log")
	foreach ($f in 'june.exe', 'junew.exe', 'june-window.exe', 'crt\msvcp140.dll', 'crt\vcruntime140.dll', 'crt\vcruntime140_1.dll', 'crt\vcomp140.dll', 'unins000.exe') {
		Assert (Test-Path (Join-Path $app $f)) "$f is installed"
	}
	Assert (Test-Path $shortcut) 'the Start menu has June'
	$shell = New-Object -ComObject Shell.Application
	$aumid = $shell.Namespace((Split-Path $shortcut)).ParseName('June.lnk').ExtendedProperty('System.AppUserModel.ID')
	Assert ($aumid -eq 'M-DEV-1.June') "the shortcut carries AppUserModelID M-DEV-1.June (got '$aumid')"
	Assert ((Get-RunValue) -like "*$app\junew.exe*--daemon*") 'start at sign-in runs the installed junew.exe'
	$config = Get-Content (Join-Path $data 'june-config.json') -Raw | ConvertFrom-Json
	Assert ($config.autostart -eq $true) "June's config says start at sign-in"
	Assert ($config.window -eq $false) 'the config written before the install was kept'

	Step 'upgrade over a running June, silent, /RELAUNCH'
	Start-Daemon
	$before = @(Get-InstalledJune | Where-Object { $_.Name -eq 'junew' })
	Assert ($before.Count -ge 1) 'the daemon runs from the install folder'
	Invoke-Program $Installer @('/VERYSILENT', '/SUPPRESSMSGBOXES', '/NORESTART', '/RELAUNCH', "/LOG=$Work\upgrade.log")
	foreach ($p in $before) { Assert (-not (Get-Process -Id $p.Id -ErrorAction SilentlyContinue)) "the old daemon ($($p.Id)) was stopped" }
	# GitHub's Windows runners run every step elevated with UAC on (their image only turns the consent prompt off), and an elevated Setup does not reopen June, which would keep those rights (SetupElevated in june.iss). There the test checks that refusal and starts June itself for the steps after.
	$uacOn = (Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System' -Name EnableLUA -ErrorAction SilentlyContinue).EnableLUA -ne 0
	$elevated = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
	if ($uacOn -and $elevated) {
		Assert ((Get-Content "$Work\upgrade.log" -Raw) -match '/RELAUNCH: June was not opened') 'an elevated Setup did not reopen June, and logged why'
		Assert (-not (Test-Ping)) 'nothing answers after an elevated upgrade'
		Start-Daemon
	} else {
		Assert (Wait-Until { Test-Ping } 60) 'June is back after the upgrade'
	}
	Assert (Test-Path (Join-Path $app 'june.exe')) 'the upgrade reused the install folder'
	Assert ((Get-RunValue) -like "*$app\junew.exe*") 'a silent upgrade left start at sign-in alone'

	Step 'june.exe --quit'
	& (Join-Path $app 'june.exe') --quit
	Assert ($LASTEXITCODE -eq 0) 'june.exe --quit exits 0'
	Assert (-not (Test-Ping)) 'nothing answers after --quit'
	Assert (Wait-Until { (Get-InstalledJune).Count -eq 0 } 15) 'no June process is left'

	Step 'uninstall while June runs keeps the data'
	# A copy kept for the next step, which needs a June that is not the installed one, as a portable copy or a development build would be.
	$elsewhere = Join-Path $Work 'elsewhere'
	New-Item -ItemType Directory -Force $elsewhere | Out-Null
	Copy-Item (Join-Path $app 'june.exe'), (Join-Path $app 'junew.exe') $elsewhere
	Start-Daemon
	Invoke-Uninstall @("/LOG=$Work\uninstall.log")
	Assert (-not (Test-Ping)) 'the uninstaller stopped June'
	Assert ((Get-InstalledJune).Count -eq 0) 'no June process is left'
	Assert (-not (Get-RunValue)) 'start at sign-in is gone'
	Assert (-not (Test-Path $shortcut)) 'the Start menu shortcut is gone'
	Assert (Test-Path (Join-Path $data 'june-config.json')) "June's data is still there"

	Step 'install without start at sign-in or update checks over a June from another folder, then uninstall with /PURGE'
	$other = Start-Process -FilePath (Join-Path $elsewhere 'junew.exe') -ArgumentList '--daemon' -WorkingDirectory $elsewhere -PassThru
	Assert (Wait-Until { Test-Ping } 60) 'a June from another folder answers /ping'
	Invoke-Program $Installer @('/VERYSILENT', '/SUPPRESSMSGBOXES', '/NORESTART', "/DIR=$app", '/MERGETASKS=!autostart,!updatecheck', "/LOG=$Work\install2.log")
	Assert (Wait-Until { $other.HasExited } 15) 'the install stopped the June running from another folder'
	Assert (-not (Get-RunValue)) 'an unticked box leaves no Run value'
	$config = Get-Content (Join-Path $data 'june-config.json') -Raw | ConvertFrom-Json
	Assert ($config.autostart -eq $false) "June's config says not to start at sign-in"
	Assert ($config.update_check -eq $false) "June's config says not to check for updates"
	Invoke-Uninstall @('/PURGE', "/LOG=$Work\uninstall2.log")
	Assert (Wait-Until { -not (Test-Path $data) } 30) '/PURGE deleted the data folder'
	Write-Host 'installer smoke test passed'
} catch {
	Get-ChildItem $Work -Filter *.log -ErrorAction SilentlyContinue | ForEach-Object {
		Write-Host "---- $($_.Name)"
		Get-Content $_.FullName -Tail 80
	}
	Get-Process june, junew, june-window -ErrorAction SilentlyContinue | Where-Object { $_.Path -and $_.Path.StartsWith("$Work\", [StringComparison]::OrdinalIgnoreCase) } | Stop-Process -Force -ErrorAction SilentlyContinue
	throw
}
