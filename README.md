<div align="center">

<img src="docs/images/banner.jpg" alt="June: Let June handle it." width="100%">

**Your computer's memory, on Windows and Linux.**

An open-source, local-first AI assistant that remembers your screen and your meetings, answers from them, and uses your computer for you.

<p>
  <img alt="Go Version" src="https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat-square&logo=go">
  <img alt="Platform" src="https://img.shields.io/badge/platform-Windows%20%7C%20Linux-0078D6?style=flat-square">
  <img alt="License" src="https://img.shields.io/badge/license-GPLv3-white?style=flat-square">
</p>

</div>

## What June does

**Remembers your screen.** June reads the window you're working in through the system's accessibility interface (UI Automation on Windows, the accessibility bus on Linux), and takes a screenshot only when an app won't say what's in it. Ask about it days later and the answer quotes the lines it came from.

**Takes your meeting notes.** It hears the call start, records both sides, and transcribes and separates speakers on your machine with whisper.cpp and sherpa-onnx. When the call ends you get what was decided, what was said and what you owe.

**Turns promises into tasks.** Anything you took on in a call lands in Tasks with the meeting beside it. Each morning brings a brief, and each evening a page on the day.

**Talks back.** `Ctrl+Alt+Space` opens the bar over any app. Type, press Space to dictate, or Shift+Space for a live voice conversation.

**Drives your desktop.** It opens apps, finds the button and clicks it, checking every step, and asks before anything it can't undo.

**Stays yours.** Local-first and open source. Memory search runs on llama.cpp on your own machine. Bring the brain you already pay for: a Gemini key, or a Claude or ChatGPT subscription.

<div align="center">

**[Install](#getting-started)** · **[Architecture](docs/architecture.md)** · **[Star on GitHub](https://github.com/M-DEV-1/june)**

</div>

## Getting started

### Windows 10 and 11

<div align="center">

**[Download June for Windows](https://github.com/M-DEV-1/june/releases/latest/download/June-Setup-x64.exe)**

<sub>Until June 0.2.0 is out, this link finds nothing: get `June-Setup-x64.exe` from the newest release candidate on the [releases page](https://github.com/M-DEV-1/june/releases).</sub>

</div>

1. Open `June-Setup-x64.exe`. It needs no admin rights and installs June for your Windows account only, on one page: what June keeps and sends, two boxes (**Start June when I sign in** and **Check for updates**, both ticked), and **Install**.
2. June opens when the installer finishes and walks you through setup: pick a brain (Claude Code, Codex or Antigravity if you are signed in to one, or a free Gemini key, which also turns on voice and sends June's memory work to Google; see [Privacy](#privacy)), test your microphone, and add local models if you want them.
3. `Ctrl+Alt+Space` opens June from any app.

**Unsigned builds.** Until a release is code-signed, Windows SmartScreen says "Windows protected your PC" when you open the installer. Click **More info**, then **Run anyway**. Each release's notes say whether it is signed, and its `SHA256SUMS` lets you check the file first: `(Get-FileHash .\June-Setup-x64.exe).Hash` in PowerShell.

**Smart App Control.** If it is on (Windows Security → App & browser control), Windows blocks programs that are not signed and offers no **Run anyway**, so an unsigned June can't be installed on that PC. Signed or not, the optional local features (voice typing, meeting transcripts, on-device models) download programs from the whisper.cpp, llama.cpp and sherpa-onnx projects, which it may block too; June's window says so before you download them.

Or install from PowerShell, which skips the SmartScreen prompt (a file PowerShell downloads isn't marked as coming from the internet) and checks the installer against the release's `SHA256SUMS` before running it. Use a normal PowerShell window, not one opened with **Run as administrator**:

```powershell
irm https://raw.githubusercontent.com/M-DEV-1/june/main/install.ps1 | iex
```

Each release also has a portable `june-windows-x86_64.zip`: unzip it anywhere and run `junew.exe`. Pre-releases, such as release candidates, are on the [releases page](https://github.com/M-DEV-1/june/releases); the download button and the install commands always take the newest full release.

### Linux

```bash
curl -fsSL https://raw.githubusercontent.com/M-DEV-1/june/main/install.sh | bash
```

1. Run the install command. It names any system libraries that are missing, sets June to start when you log in (you can turn that off in setup or in June's settings), and opens June when it is done.
2. June walks you through setup: a brain (a Gemini API key, or a Claude or ChatGPT subscription), your microphone, and optional local models.
3. Log out and back in once, so GNOME loads June's window extension, which computer use needs.
4. Run `june doctor` at any time. It lists anything still missing.

## Privacy

June runs on your computer and keeps its memory there: the database, recordings, screenshots and config, in `%LOCALAPPDATA%\june` on Windows and `~/.local/share/june` on Linux. June has no analytics, no telemetry and no June account; its developers receive nothing.

- **What June observes.** After you finish first-run setup, June notes the app and window you are using and the text on screen, and takes a screenshot when an app won't say what's in it. Common password managers are skipped. Nothing is observed before setup is done, and Pause observation in the tray menu stops it at any time.
- **Meetings** are recorded only when you say so. Transcription and speaker separation run on your machine.
- **The brain you pick.** Your questions, and the parts of your memory needed to answer them, go to the AI service you chose in setup (Claude Code from Anthropic, Codex from OpenAI, Antigravity or Gemini from Google), under your own account and that service's privacy terms. If that brain is out of allowance or signed out, June asks the next of those services you are signed in to on this computer. A task June carries out on your screen that the picked brain can't finish goes on to Claude Code, if you are signed in to it.
- **Background work.** Meeting minutes, the morning brief, the evening close and overnight notes go to the brain you picked, and stay with it after you add on-device models.
- **Memory.** Turning what June observes into memory (summaries of what you did, threads, notes and their upkeep) goes to Google's Gemini API whenever a Gemini API key is set, whichever brain you picked. Without a key it runs on your machine once you add the optional on-device text model; with neither, it is not done and June keeps only the raw activity log. The short note of what you are working on right now runs on the on-device model whenever one is added, key or not, and goes to the brain you picked when that model can't answer.
- **When background work can't be answered** (a spent allowance, an expired login), June hands it to another of those services you are signed in to on this computer. Turn that off in setup or in Settings → Brain ("If your chosen AI can't answer, try my other signed-in AIs"), and background work goes only to the brain you picked, and to Gemini for memory when a key is set. The switch covers background work only: it does not stop a question being handed on, or a task going on to Claude Code. Grok (xAI) is only ever used if you choose it yourself.
- **With a Gemini API key**, a voice conversation streams your microphone, and the parts of your memory needed to answer, to Google while it is open, and screenshots June can't read as text are described by Gemini.
- **With an Exa or Tavily key**, web searches go to that service.
- **Plan usage.** To show how much of each plan is left, June asks the services you are signed in to (Anthropic, OpenAI, Google for Antigravity, xAI, Tavily) for your account's usage and whether the login still works. Nothing from your memory goes with it. Settings has a switch for the Claude one, which reads an undocumented Anthropic endpoint.
- **Other connections.** Once a day June asks GitHub whether a newer release is out. Untick **Check for updates** in the Windows installer, or turn it off in Settings, and it only checks when you ask. Optional local models download from GitHub and Hugging Face only when you ask for them.

The Windows installer shows a short form of this notice on its only page, before it installs anything. Uninstalling asks whether to delete your June data too.

## Uninstall

**Windows:** Settings → Apps → Installed apps (Apps & features on Windows 10) → June → Uninstall. It asks whether to delete your June data (memory, recordings, downloaded models) as well; the default keeps it. A silent uninstall, `%LOCALAPPDATA%\Programs\June\unins000.exe /VERYSILENT`, keeps the data unless you add `/PURGE`.

**Linux:**

```bash
curl -fsSL https://raw.githubusercontent.com/M-DEV-1/june/main/packaging/uninstall.sh | bash
```

This removes June's files and leaves your data folder.

## Build from source

Needs Go 1.25+, Rust and pnpm.

```bash
git clone https://github.com/M-DEV-1/june.git && cd june
make release
```

On Windows, with Rust's MSVC toolchain and, for the installer, [Inno Setup 7](https://jrsoftware.org/isinfo.php):

```powershell
powershell -ExecutionPolicy Bypass -File packaging\release-windows.ps1
```

It leaves `June-Setup-x64.exe` and `june-windows-x86_64.zip` in `dist\windows`.

## Code signing policy

Free code signing provided by [SignPath.io](https://about.signpath.io), certificate by [SignPath Foundation](https://signpath.org).

- Committers and reviewers: [@M-DEV-1](https://github.com/M-DEV-1)
- Approvers: [@M-DEV-1](https://github.com/M-DEV-1)

Only the release workflow in this repository (`.github/workflows/release.yml`), running on GitHub's own runners, submits files for signing, and every release-signing request is approved by hand. Signed are June's own programs (`june.exe`, `junew.exe`, `june-window.exe`), the installer `June-Setup-x64.exe`, and the Inno Setup program inside it that runs the install and stays behind as the uninstaller `unins000.exe`; the Microsoft Visual C++ runtime files shipped beside them are Microsoft's and keep Microsoft's signature. What June sends and where is described under [Privacy](#privacy).

## License

GPLv3
