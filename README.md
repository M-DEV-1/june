<div align="center">
  <img src="./docs/images/logo.png" width="600" alt="ORA Logo">

  <p align="center">
    <img alt="Go Version" src="https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat-square&logo=go">
    <img alt="Platform" src="https://img.shields.io/badge/platform-Linux-0078D6?style=flat-square">
    <img alt="License" src="https://img.shields.io/badge/license-GPLv3-white?style=flat-square">
  </p>

# ORA

from Latin _orare_. To speak, to ask.

</div>

Ora is a memory for the person using this computer. It watches what is on screen, listens to the meetings you record, keeps what happened, and answers questions about it later. It is not a coding assistant and not a search engine: if you want general knowledge, use a browser.

It runs as one Go daemon on your own machine, with a SQLite store beside it. Nothing is sent anywhere except the prompts you or its own duties send to whichever model you pick.

## What it does today

**Watches.** A tracker reads the focused window through AT-SPI, the accessibility bus, and falls back to a screenshot and a vision model only when the window will not say what is in it — Electron apps, canvas, anything the bus is blind to. Every read is one episode row: the app, the window title, the text, the time.

**Records meetings.** A watcher notices a call is running, records both sides, transcribes with whisper.cpp, and separates the speakers with sherpa diarization. The transcript then goes to a model that writes minutes: what you said, what was said to you, what the meeting covered, what was decided, and who owes what. The owed work becomes tasks.

**Remembers.** Episodes, notes, meeting minutes and day pages go into SQLite. Retrieval is hybrid — FTS5 for words, a local vector index for meaning, fused and reranked by importance and recency rather than by similarity alone. Memory is editable: a fact heard wrong can be corrected in the conversation it came up in, instead of sitting there forever.

**Consolidates while idle.** A dreaming pass reads the day back, forms hypotheses about what matters, writes a diary entry in the evening and a brief in the morning.

**Speaks.** Dictation goes through whisper; a live voice session runs on Gemini Live, with a waveform drawn in braille characters in the hover window.

**Acts.** A job engine can carry out a screen task — open this, find that, click it — through the portal input APIs, with a verify loop, a budget, and a stop line it will not cross without being asked.

Rough edges, plainly: diarization still mislabels speakers in a crowded call, the live voice session needs a working `GEMINI_API_KEY` and says nothing useful when it does not have one, and the usage panel only updates when a call spends something.

## The surfaces

| Surface | What it is | How you reach it |
| --- | --- | --- |
| Daemon | Go, HTTP on `127.0.0.1:6942`, SQLite store | `ora --daemon`, or the systemd user unit |
| Window | Tauri + React desktop app: chats, meetings, days, tasks, routines, settings | starts with the daemon |
| Hover | one small always-there card: ask, dictate, live voice, notices | `Ctrl+Alt+Space` |
| Overlay | click-through layer the act engine draws on | drawn only while a job runs |
| Terminal | the original TUI client | `ora --tui` |

## Running it

```bash
git clone https://github.com/M-DEV-1/ora.git
cd ora
```

Put your key in `.env` at the repo root:

```env
GEMINI_API_KEY=your_key_here
```

Then:

```bash
go build -o ora . && ./ora
```

`ora` brings up the daemon and the window together and says so. The build is CGO-free and Linux-only: the Windows and macOS build files were removed once the target became one machine.

## Which model answers

Ora keeps several backends and lets you pick per duty, because they are billed differently and only some of them are metered.

- **Antigravity** (`agy`) — Gemini under your own Google plan login. Nothing is charged against an API key.
- **Gemini API** — the metered key in `.env`. Fast, and the only backend the live voice session and the screen agent can use, because they need streaming and tool calls.
- **Codex** — OpenAI models on your ChatGPT login, through the Codex CLI.
- **Ollama** — a model on this machine. Nothing leaves the laptop.
- **Claude** — Anthropic's own CLI on your subscription. Last in the list on purpose: it is the fallback, after Gemini and Codex.

Embeddings are local: EmbeddingGemma-300M served by Ollama, called over HTTP so the build stays CGO-free.

## Where your data is

Everything lives under `~/.local/share/ora`: the SQLite store, the config, recordings, screenshots. Config is `ora-config.json` in that directory. Retention is set there — how many days of audio and images to keep before they are aged out.

## Tracing

Ora exports OpenTelemetry traces over OTLP/gRPC. Run Jaeger and they appear at `http://localhost:16686`:

```bash
docker run cr.jaegertracing.io/jaegertracing/jaeger:2.17.0
```

## Status

One machine, one user, no installer. The daemon and the window run all day and the memory answers, but nothing here is packaged for anybody else yet. The tests are the record of what is meant to hold: about 1,600 Go test functions and 680 in the window.
