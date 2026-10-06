<!-- Template for the top of every GitHub release's notes. The release workflow fills in {{VERSION}} and {{REPO}}, drops the unsigned block when the Windows programs were signed, and GitHub's generated list of changes follows. -->
## Install

**Windows 10 or 11 (64-bit):** download [June-Setup-x64.exe](https://github.com/{{REPO}}/releases/download/v{{VERSION}}/June-Setup-x64.exe) and open it. It needs no admin rights, installs June for your Windows account only, and opens June when it is done; June then walks you through a short setup.

From PowerShell instead (it checks the download against `SHA256SUMS` before running it):

```powershell
irm https://raw.githubusercontent.com/{{REPO}}/main/install.ps1 | iex
```

Portable: unzip `june-windows-x86_64.zip` anywhere and run `junew.exe`. It needs the Microsoft Edge WebView2 Runtime, which Windows 11 already has.

**Linux (x86_64):**

```sh
curl -fsSL https://raw.githubusercontent.com/{{REPO}}/main/install.sh | bash
```

The two one-line installers always fetch the newest full release, never a pre-release.

<!-- unsigned:start -->
### This Windows build is not code-signed yet

Windows SmartScreen will say "Windows protected your PC". Click **More info**, then **Run anyway**. If you want to check the file first, compare its checksum with `SHA256SUMS` as shown below.

If Windows instead says **Smart App Control** blocked it, there is no Run anyway: Smart App Control lets no unsigned program run, and cannot make an exception for one app. Either wait for a signed release of June, or turn Smart App Control off in Windows Security → App & browser control → Smart App Control settings. Windows 11 with its 2026 updates lets you turn it back on later; on older builds, turning it off lasts until Windows is reinstalled.
<!-- unsigned:end -->

### Check your download

- Windows (PowerShell): `(Get-FileHash .\June-Setup-x64.exe).Hash` should match the `June-Setup-x64.exe` line of `SHA256SUMS`.
- Linux: `sha256sum --check --ignore-missing SHA256SUMS`
- Where it was built: `gh attestation verify June-Setup-x64.exe --repo {{REPO}}` confirms the file came from this repository's release workflow.

What June keeps and what it sends where: [privacy statement](https://github.com/{{REPO}}#privacy).
