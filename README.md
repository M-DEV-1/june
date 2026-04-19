# ORA

ORA is a high-performance, low-latency TUI (Terminal User Interface) voice agent powered by the Gemini 2.0 Multimodal Live API. It enables autonomous research through real-time voice interaction.

## Key Features

- **Autonomous Research Loop**: Built-in specialized agent that can search, read web pages, synthesize information, and save research notes autonomously to your local machine.
- **Low-Latency Streaming Audio**: Optimized audio pipeline for immediate interruption handling and real-time response.
- **"Command Center" UI**: A high-density, polished TUI built with Ink.
- **On-Device Persistence**:
  - API keys are securely loaded from your environment or `.env` file.
  - Research findings are automatically organized in `~/.ora/findings/`.

## Getting Started

1.  **Clone the repository**
2.  **Install dependencies**:
    ```bash
    pnpm install
    ```
3.  **Run the application**:
    ```bash
    pnpm start
    ```
    _Note: Ensure you have `GEMINI_API_KEY` set in your environment or a `.env` file in the root._

## âŒ¨ï¸ Controls

- **[^M]**: Toggle mute.
- **[^R]**: Reset session.
- **[^C]**: Exit gracefully.

## Project Structure

- `packages/cli`: The TUI application built with React and Ink.
- `packages/core`: Core audio processing, ADK orchestration, and memory management.
