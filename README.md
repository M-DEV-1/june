<div align="center">

# June

**Your computer's memory, on Linux.**

An open-source, local-first AI assistant that remembers your screen and your meetings, answers from them, and uses your computer for you.

<p>
  <img alt="Go Version" src="https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat-square&logo=go">
  <img alt="Platform" src="https://img.shields.io/badge/platform-Linux-0078D6?style=flat-square">
  <img alt="License" src="https://img.shields.io/badge/license-GPLv3-white?style=flat-square">
</p>

```bash
curl -fsSL https://raw.githubusercontent.com/M-DEV-1/june/main/install.sh | bash
```

<img src="docs/images/hero.jpg" alt="June answering a question from an email it read that morning, with the lines it used highlighted" width="100%">

</div>

## What June does

- **Screen memory.** Reads the window you're in. Ask about it later and see the lines it used.
- **Meeting notes.** Records calls and transcribes them on your machine. You get decisions and who owes what.
- **Tasks.** Promises from calls, with the call they came from.
- **Voice.** `Ctrl+Alt+Space` opens the bar. Space dictates, Shift+Space talks.
- **Computer use.** Opens apps and clicks through them. Asks before anything it can't undo.
- **Local-first.** Data stays in `~/.local/share/june`. Bring your own Gemini, Claude or ChatGPT.

<div align="center">

**[Install](#getting-started)** · **[Architecture](docs/architecture.md)** · **[Star on GitHub](https://github.com/M-DEV-1/june)**

</div>

## Getting started

1. Run the install command above. It names any system libraries that are missing.
2. Log out and back in, so GNOME loads June's window extension.
3. Add a brain: a Gemini API key, or a Claude or ChatGPT subscription.
4. Run `june doctor`. It lists anything still missing.

## Your data

Everything stays in `~/.local/share/june`: the database, recordings, screenshots and config. Meeting transcription, speaker separation and memory search run on your machine. The only thing sent out is a request to the brain you picked.

## Uninstall

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

## License

GPLv3
