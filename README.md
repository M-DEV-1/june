<div align="center">
  <img src="./docs/images/logo.png" width="600" alt="ORA Logo">

  <p align="center">
    <img alt="Go Version" src="https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat-square&logo=go">
    <img alt="Platform" src="https://img.shields.io/badge/platform-Linux%20|%20Windows-0078D6?style=flat-square">
    <img alt="License" src="https://img.shields.io/badge/license-GPLv3-white?style=flat-square">
  </p>

# ORA

Ora is a thin, Go-native OS companion.

from Latin _Orare_. To speak, to know, to ask of what cannot be seen.

It monitors your workspace activity locally and provides voice-first contextual assistance from your terminal and desktop environment.

---

</div>

If you want general knowledge, use a browser.

ORA is for:

- your terminal
- your windows
- your active workflow
- your local context

## Why?

Modern assistants know the internet but barely know about your actual working context. The ones that do exist are heavy, and your data is god knows where.

I experimented with a different model:
persistent local context, lightweight telemetry, and terminal-native interaction.

## Usage

### Downloads

Prebuilt Windows binaries are available in GitHub Releases.

### Nightly

```bash
git clone https://github.com/M-DEV-1/ora.git;
cd ora
```

Add your Gemini API key to .env:

```env
GEMINI_API_KEY=your_key_here
```

```
go run .
```

### Observability

Run [Jaeger](https://www.jaegertracing.io/) for observing (optional, but recommended):

```bash
 docker run cr.jaegertracing.io/jaegertracing/jaeger:2.17.0 --help
```

ORA exports OpenTelemetry traces over OTLP/gRPC.

If Jaeger is running locally, traces will automatically appear at http://localhost:16686

## Features

- voice-first terminal interaction
- switchable voice persona (`/voice list`, `/voice preview <name>`)
- local workspace activity tracking (Linux AT-SPI · Windows UIA)
- tiered capture: accessibility text first, screenshot + vision only when blind
- media-aware capture (MPRIS), sees you're watching or in a call, not just the chrome
- episodic memory, i.e. raw captures are preserved, then recalled ("walk me through my day" and other stuff like this)
- hybrid retrieval (FTS5 + vector search over local embeddings), memory is fully CRUD so a misheard/wrong fact can be corrected in the same conversation instead of sticking around forever
- OpenTelemetry tracing support
- TUI interface
- persistent lightweight context tracking
- fully written in Go (no CGO)

## Notes

Windows event tracking turned out to be significantly harder than the LLM integration itself. But oh boy, it was fun.

Most of the complexity comes from process synchronization, audio pipes, and keeping threads from fighting each other. Hopefully, it was lightweight enough to run continuously without becoming intrusive.

## Plans?

- [x] Linux tracker support
- [x] Lightweight vector recall (went with chromem + Gemini embeddings, fused with FTS5 instead of hnsw)
- [ ] Local model support
- [ ] Sandboxed automation runtime
