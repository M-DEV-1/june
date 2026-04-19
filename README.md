# VOXPILOT - Production-Grade Voice Research Agent

VOXPILOT is a high-performance, low-latency TUI (Terminal User Interface) voice agent powered by the Gemini 2.5 Multimodal Live API. It enables autonomous research through real-time voice interaction.

## 🚀 Key Features

- **Autonomous Research Loop**: Built-in specialized agent that can search, read web pages, synthesize information, and save research notes autonomously to your local machine.
- **Zero-Dependency Streaming Audio**: Implements a custom, platform-agnostic `ffplay`-based audio stream that guarantees ultra-low latency and immediate interruption handling *without* requiring `node-gyp` or pre-installed system dependencies.
- **"Command Center" UI**: A high-density, polished TUI inspired by modern CLI tools like Claude Code and Gemini CLI.
- **On-Device Persistence**: 
  - API keys are securely saved to a local `.env` file to skip future setups.
  - Raw conversation transcripts are continuously logged to `~/.voxpilot/sessions/`.
  - Research findings are automatically organized in `~/.voxpilot/findings/`.
- **Dual Implementations**: Choose between the ADK-based engine (high-level orchestration) or the GenAI-based session (direct Live API control).

## 🛠 Project Structure

- `voxpilot-adk/`: Implementation using the Agent Development Kit (ADK). Best for complex multi-agent workflows.
- `voxpilot-genai/`: Implementation using the standard @google/genai SDK. Best for low-level Live API control and maximum performance.
- `shared/`: Shared UI components and optimized audio/api utilities.

## 🏁 Getting Started

1.  **Clone the repository**
2.  **Install dependencies**:
    ```bash
    pnpm install
    ```
3.  **Run the application**:
    ```bash
    # To run the ADK engine:
    npm run adk
    
    # To run the GenAI engine:
    npm run genai
    ```
    *Note: On your first run, you will be prompted for your Gemini API key, which will be saved for future sessions.*

## ⌨️ Controls

- **[1-4]**: Toggle visualization modes (Oscilloscope, Bars, Radial, Particles).
- **[C]**: Cycle through UI color palettes (Cyberpunk, Neon, Aurora, Sunset).
- **[^C]**: Exit gracefully.

## ⚠️ Known Issues & Architecture Notes

- **Audio Subsystem**: The app dynamically downloads static binaries for `ffmpeg` and `ffplay` into `~/.voxpilot/bin/` to ensure consistent audio capture and playback across all operating systems without native build failures.
- **Latency Monitoring**: The TUI includes real-time session latency monitoring to help debug network conditions.
