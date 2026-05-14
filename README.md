<div align="center">
  <img src="./docs/images/logo.png" width="600" alt="ORA Logo">

  <p align="center">
    <img alt="Go Version" src="https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat-square&logo=go">
    <img alt="Platform" src="https://img.shields.io/badge/platform-Windows-0078D6?style=flat-square">
    <img alt="License" src="https://img.shields.io/badge/license-MIT-white?style=flat-square">
  </p>

# ORA

Ora is a thin, Go-native OS companion.

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
- local workspace activity tracking
- OpenTelemetry tracing support
- TUI interface
- persistent lightweight context tracking
- fully written in Go (no CGO)

## Notes

Windows event tracking turned out to be significantly harder than the LLM integration itself. But oh boy, it was fun.

Most of the complexity comes from process synchronization, audio pipes, and keeping threads from fighting each other. Hopefully, it was lightweight enough to run continuously without becoming intrusive.

## Plans?

- [ ] Linux tracker support
- [ ] Local model support
- [ ] Sandboxed automation runtime
