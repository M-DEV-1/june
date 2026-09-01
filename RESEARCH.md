# ORA — Technical Research

## 1. Screen Capture

### Problem Space

ORA already calls Win32 APIs via `syscall.NewLazyDLL` / `NewProc` (see `tracker_windows.go`). Screen capture follows the identical pattern — `gdi32.dll` and `user32.dll` are loadable the same way. CGO is not required on Windows, and ORA's existing calling convention is a proven template.

---

### Windows Options

#### Option A: GDI BitBlt ✓ Recommended baseline

BitBlt creates a compatible device context, allocates an off-screen bitmap, copies the screen into it, reads pixel data via `GetDIBits`.

**Pure Go, no CGO.** Two paths:
- `github.com/lxn/win` — pure-Go Win32 bindings. Exposes `BitBlt`, `CreateCompatibleDC`, `CreateCompatibleBitmap`, `GetDIBits`, `GetDC`, `ReleaseDC`, `DeleteDC`, `DeleteObject`.
- Raw `syscall.NewLazyDLL("gdi32.dll")` — identical to what `tracker_windows.go` already does.

**Full screen:** `GetDC(0)` (NULL HWND)
**Specific window:** `GetDC(hwnd)` — combine with `GetForegroundWindow` from the existing tracker
**Minimized/obscured window:** `PrintWindow(hwnd, hdcMem, PW_RENDERFULLCONTENT)` — works for Electron, UWP, most apps; does not work for DRM-protected content

**Performance:** 5–30ms per 1080p frame. At 1–2 fps (all ORA needs), CPU cost is negligible (<1% sustained).

**Limitation:** Does not capture DRM-protected content (Netflix, some video players).

---

#### Option B: `github.com/kbinani/screenshot` ✓ Recommended cross-platform library

| Platform    | CGO Required | Underlying API    |
|-------------|-------------|-------------------|
| Windows     | No          | GDI BitBlt        |
| Linux X11   | No          | XGetImage         |
| macOS       | **Yes**     | CoreGraphics      |
| Linux Wayland | Partial (shell-out) | grim / portal |

**API:** `screenshot.CaptureDisplay(n)`, `screenshot.CaptureRect(rect)` → `*image.RGBA`

For window-specific capture: get bounds from `GetWindowRect(hwnd)`, pass to `CaptureRect`. ORA already tracks the active HWND.

**Binary size impact:** Negligible — pure Go, no additional deps on Windows/Linux.

This is the pragmatic choice: works today on Windows with zero CGO; Linux X11 works identically; macOS deferred.

---

#### Option C: DXGI Desktop Duplication — Skip for now

DXGI is the modern GPU-side approach (Windows 8+). Near-zero CPU for high-fps capture but requires complex COM interface sequencing (`IDXGIOutput1::DuplicateOutput`, `IDXGIOutputDuplication::AcquireNextFrame`, etc.). No maintained pure-Go DXGI capture library exists.

**For ORA (1–2 fps context snapshots):** BitBlt is indistinguishable in CPU cost. Defer DXGI unless ORA needs delta detection or >5 fps.

---

### Linux

- **X11:** `github.com/jezek/xgb` (pure Go X11) or `kbinani/screenshot`. Fine for ORA's snapshot frequency.
- **Wayland:** No pure-Go path. Shell out to `grim -o -` (wlroots compositors) or `gnome-screenshot --file=/tmp/cap.png` (GNOME). Read PNG from stdout/file. Pragmatic; avoids D-Bus complexity.

---

### macOS

All options require CGO (`CGWindowListCreateImage` via CoreGraphics). Also requires Screen Recording permission (TCC) — first access triggers a system dialog.

**Decision:** Defer macOS behind a `//go:build !darwin` tag with a stub. Implement when macOS becomes a target.

---

### OCR — Use Gemini Vision, Not a Local Library

| Option | CGO | Offline | Binary Impact | Accuracy | Verdict |
|--------|-----|---------|--------------|----------|---------|
| gosseract (Tesseract) | Yes | Yes | +system libs | Good | Reject — breaks no-CGO |
| Pure Go OCR | No | Yes | Minimal | Unusable | Does not exist at production quality |
| **Gemini Vision API** | **No** | **No** | **Zero** | **Superior** | **Use this** |

ORA already has `google.golang.org/genai`. Sending a captured frame for text extraction is one additional call:

```go
var buf bytes.Buffer
jpeg.Encode(&buf, resizedFrame, &jpeg.Options{Quality: 70})

resp, _ := client.Models.GenerateContent(ctx, "gemini-2.0-flash", genai.Parts{
    genai.Text("Extract all visible text from this screenshot. Return plain text only."),
    &genai.BlobPart{MimeType: "image/jpeg", Data: buf.Bytes()},
}, nil)
```

Resize to 1280px wide (via `golang.org/x/image/draw`, pure Go) before sending to reduce payload from ~3MB PNG to ~150KB JPEG. Rate-limit to 1 call per 5–10 seconds.

---

### Recommended Screen Capture Architecture (Windows-first)

```
tracker daemon — on new activity event
  └─ kbinani/screenshot.CaptureDisplay(0)
      └─ golang.org/x/image/draw → resize to 1280×720
          └─ jpeg.Encode at Q=70
              └─ Gemini Vision → extract visible text
                  └─ inject into system prompt alongside GetImplicitContext()
                     OR expose as new tool: read_screen
```

**Implementation path:**
1. Add `github.com/kbinani/screenshot` to `go.mod`
2. New file `internal/capture/capture_windows.go` with build tag
3. New `read_screen` tool in `agent/tools.go` — captures, OCRs, returns text
4. OR: inject screen text into system prompt context in `connect.go` alongside activity summaries

---

## 2. Vector Search / Semantic Memory

### Current System

`QueryMemory` in `db.go` does `WHERE type = 'summary' AND content LIKE ?` — substring matching on JSON-serialized task summaries. Misses semantic equivalents ("reviewed pull request" vs "what code did I review?"). Degrades with volume.

The **temporal tree** (USER→DAY→SESSION→TASK→SUMMARY) is valuable for hierarchical navigation and context window construction (`GetImplicitContext`). Vector search **augments** retrieval, not replaces the tree.

---

### Options

#### `github.com/coder/hnsw` ✓ Recommended

- **Pure Go, zero CGO.** 15 stdlib imports only.
- **Algorithm:** HNSW (Hierarchical Navigable Small World) — same algorithm as Pinecone, Weaviate, FAISS. O(log n) insert and search.
- **API:** Generic `Graph[K]`. `Add(nodes...)`, `Search(vector, k)`, `Delete(key)`, `Export(w)`, `Import(r)`.
- **Persistence:** `SavedGraph[K]` persists every mutation to a file automatically.
- **Distance functions:** `CosineDistance`, `EuclideanDistance` as `float32`.
- **At ORA scale:** 1,000 summaries × 768 dims ≈ 3MB memory. Search latency <1ms. Insert latency <1ms.
- **Status:** v0.6.1, pre-stable — pin version in `go.mod`.

#### `go.etcd.io/bbolt` — Storage layer only, not needed

Pure Go B-tree KV. No vector search. Could persist raw embeddings alongside HNSW, but `SavedGraph[K]` already handles HNSW persistence. No need to add bbolt.

#### `sqlite-vss` / `sqlite-vec` — Reject

Requires loading a native extension into SQLite at runtime. `modernc.org/sqlite` (ORA's pure-Go driver) does not support native extensions — that requires `mattn/go-sqlite3` (CGO). Breaks no-CGO constraint.

#### pgvector, Weaviate — Reject

External server dependencies. Incompatible with a self-contained desktop binary.

#### `github.com/philippgille/chromem-go` — Secondary option

Pure Go, document-centric API (higher-level than raw HNSW). Less control, less performance. Use if `coder/hnsw` API proves too low-level.

---

### Embedding Generation

Use **Gemini `text-embedding-004`** — already in ORA's SDK:

```go
resp, err := client.Models.EmbedContent(ctx, "text-embedding-004",
    genai.Text(summary.TaskName + ". " + summary.Summary),
    nil)
// resp.Embeddings[0].Values is []float32, 768 dimensions
```

- **Free tier:** 1,500 req/min, 1M req/day — far exceeds ORA's usage (~10–20 embeddings/hour)
- **Latency:** 50–150ms — called async after `LogSemanticNode`, never on critical path
- **Quality:** Excellent for task-description semantic similarity

---

### When Vector Search Beats LIKE

| Scenario | LIKE | Vector |
|----------|------|--------|
| "What did I work on today?" | ✓ | ✓ |
| "When did I last debug auth?" (phrased differently than stored) | ✗ | ✓ |
| "Show me everything database-related" across weeks | ✗ | ✓ |
| <50 summaries | LIKE is fine | Overkill |
| >100 summaries (~3–4 weeks of daily use) | Degrades | ✓ |

Build the embedding pipeline now; the data is ready when volume warrants it.

---

### Recommended Architecture

**Layer 1 (existing):** SQLite temporal tree — source of truth for all nodes, context window construction.

**Layer 2 (add):** `coder/hnsw` `SavedGraph[string]` persisted to `ora-db/vectors.hnsw`. Key = SQLite node ID as string (e.g., `"1234"`).

**Layer 3 (add):** Async embedding goroutine triggered after each `LogSemanticNode`.

**Modified `QueryMemory`:**
```
query
  ├─ embed via Gemini text-embedding-004 (~100ms)
  ├─ graph.Search(queryVec, k=10) → node IDs
  ├─ load content from SQLite by IDs
  └─ return
  
fallback (offline / embed failure):
  └─ existing LIKE search (keep, never remove)
```

**Integration in `db.go`:**
- Add `vectorIdx *hnsw.SavedGraph[string]` to `Store`
- Load in `New()` from `ora-db/vectors.hnsw`
- `LogSemanticNode` spawns goroutine: embed → `vectorIdx.Add(nodeID, vec)`
- `QueryMemory` tries vector path first, falls back to LIKE

---

### Summary

| Dimension | Screen Capture | Vector Search |
|-----------|---------------|---------------|
| Library | `kbinani/screenshot` | `coder/hnsw` |
| CGO | No (Windows/Linux) | No |
| Text extraction | Gemini Vision API | Gemini text-embedding-004 |
| Binary size impact | Negligible | Negligible |
| Offline | Capture yes, OCR no | Index yes, embed no |
| Implementation effort | Medium | Medium |
| Priority | High (core feature gap) | Medium (quality improvement) |
# Ora Memory Architecture: HNSW Vector Search

This document outlines the dual-path architectural strategy for migrating Ora's memory retrieval from primitive SQL `LIKE` queries to high-dimensional Semantic Vector Search using Hierarchical Navigable Small World (HNSW) graphs. 

## 1. Executive Summary
Vector embeddings map semantic meaning to mathematical space, allowing Ora to understand that "messaging a friend" maps to "Discord chat." To achieve this without bloated C++ database extensions (like `sqlite-vec`), we will use `github.com/coder/hnsw`—a pure Go, highly concurrent, generic HNSW implementation.

We are designing for two deployment paths:
*   **Path A: Hybrid (API-Based)** - Maximum intelligence, minimum binary size (~20MB).
*   **Path B: Local (ONNX-Based)** - 100% offline, requires external runtimes (~150MB footprint).

---

## 2. Path A: Hybrid (API-Based) Implementation

In this path, the heavy lifting of mathematical semantic mapping is outsourced to Google.

### Implementation Flow
1.  **Ingestion:** The Memory Compiler generates a text summary -> Calls `text-embedding-004` API -> Receives a `[]float32` vector (768 dimensions).
2.  **Storage:** The 768-dim vector is serialized to bytes and stored in an SQLite `BLOB` column alongside the summary text.
3.  **Indexing:** The vector is simultaneously inserted into the in-memory `coder/hnsw` graph.
4.  **Retrieval:** The `query_memory` tool sends the user's query to the API, gets a query vector, and runs `hnsw.Search()` against the in-memory graph using Cosine Similarity.

### Resource Costs
*   **CPU Utilization:** **Negligible (~0.1%).** Generating embeddings requires zero local math. The HNSW distance calculation (Cosine Similarity) on a 768-dim array is trivial for modern CPUs, taking microseconds per query.
*   **RAM Cost:** **Extremely Low.** The binary remains ~20MB.
*   **Latency Cost:** Requires two network round-trips for the `query_memory` tool (one for the embedding, one for the generation).

---

## 3. Path B: Local (ONNX-Based) Implementation

In this path, Ora generates embeddings entirely offline on the host machine.

### Implementation Flow
1.  **Opt-In Engine:** To preserve the "Thin Binary" for default users, Local Mode must be an opt-in toggle.
2.  **Runtime Management:** Ora downloads `onnxruntime.dll` and the `all-MiniLM-L6-v2.onnx` model (384 dimensions) to a hidden `~/.ora/models/` folder.
3.  **Ingestion:** Uses `github.com/yalue/onnxruntime_go` (CGO required) to pass the summary string to the local ONNX engine.
4.  **Storage & Indexing:** Similar to Path A, but vectors are 384 dimensions.

### Resource Costs
*   **CPU Utilization:** **High Bursts (~15-30% on single core).** Running a transformer inference pass locally requires intense matrix multiplication. Embedding a chunk of text will take ~10-50ms of heavy CPU blocking.
*   **RAM Cost:** **High Baseline (+150MB).** Loading the ONNX runtime and the `MiniLM` weights into memory will permanently increase Ora's idle footprint.
*   **Deployment Cost:** Breaks the pure-Go ecosystem, requiring architecture-specific `.dll` or `.so` files to be distributed or downloaded.

---

## 4. HNSW Graph Memory Management Over Time

The HNSW algorithm works by keeping the vector index entirely in RAM for hyper-fast retrieval. 

### The Memory Leak Problem
If Ora runs for 3 years and logs 50,000 memories, the in-memory graph will continuously expand.
*   A 768-dimensional float32 vector = ~3KB.
*   An HNSW node overhead (edges/pointers) = ~1KB.
*   **Cost per memory:** ~4KB.
*   **10,000 memories:** ~40MB of pure RAM overhead.

### The Management Strategy
We must prevent the HNSW graph from consuming unlimited RAM.
1.  **Temporal Pruning (Sliding Window):** The HNSW graph should only index the last $N$ days (e.g., 30 days) of active memory or a fixed cap of 10,000 nodes.
2.  **Disk Serialization:** `coder/hnsw` supports Export/Import. On shutdown, we serialize the graph to a `graph.bin` file in the `ora-db` directory.
3.  **Boot Rehydration:** On startup, Ora loads `graph.bin`. If the file is missing or corrupted, Ora performs a cold-start by reading the last 10,000 rows from the SQLite `BLOB` column and rebuilding the graph in a background goroutine.
4.  **Cold Storage:** Older memories (beyond the 10k limit) fall out of the active HNSW graph but remain permanently stored in the SQLite DB. They can be retrieved via traditional lexical fallback if needed.

---

## 5. Detailed Analysis: RAM Usage vs. Time Active

Because Ora operates as a background daemon (auto-forked on startup), its long-term stability relies on strict memory bounds. If the daemon leaks memory or scales linearly with uptime, the OS will eventually kill it or the user will uninstall it. 

Here is the projected RAM utilization curve for the Daemon as it remains active over time, specifically concerning the HNSW graph and SQLite WAL.

### 5.1 The Baseline (0 to 24 Hours)
*   **Idle Footprint:** ~15 MB - 20 MB.
*   **Audio Buffers:** The background daemon drops audio contexts and only runs the `Tracker`. 
*   **Memory Compiler:** Wakes up every 10 app-switches, creating short-lived structs. Go's Garbage Collector (GC) easily reclaims this within milliseconds.
*   **HNSW Graph:** At ~100 summaries per day, the graph adds ~400 KB of vector data per 24-hour cycle.
*   **Net RAM Growth:** Virtually zero. The GC keeps the process anchored at ~20 MB.

### 5.2 The Medium Term (1 Week to 1 Month)
*   **Accumulation Phase:** After 30 days of heavy use, Ora generates ~3,000 memory nodes.
*   **HNSW Graph Size:** 3,000 nodes × ~4 KB (Vector + Edges) = **12 MB**.
*   **Total RAM Usage:** ~30 MB to 35 MB.
*   **Go GC Behavior:** Because the HNSW graph is an active, in-memory pointer structure, Go's GC cannot reclaim it. The heap size permanently increases by the exact size of the graph. However, because `coder/hnsw` uses generics and contiguous arrays rather than millions of interface pointers, GC scan times remain incredibly fast (sub-millisecond), preventing CPU spikes.
*   **SQLite WAL Growth:** The Write-Ahead Log may temporarily inflate the memory footprint by a few megabytes during heavy compiler activity, but SQLite automatically checkpoints the WAL file every 1000 pages, flushing it to disk and releasing RAM.

### 5.3 The Long Term (6 Months to 3 Years)
If left completely unmanaged, the RAM curve would scale linearly forever.
*   **1 Year:** ~36,500 nodes = ~146 MB.
*   **3 Years:** ~110,000 nodes = ~440 MB.

While 440 MB is acceptable for an Electron app, it violates Ora's "Thin Binary" philosophy. To guarantee Ora never exceeds **50 MB** of RAM regardless of uptime, we enforce the **Temporal Pruning Boundary**.

### 5.4 The 50MB Horizon (Temporal Pruning Execution)
We implement a hard cap on the `coder/hnsw` graph: **$K_{max} = 10,000$ active nodes.**
*   **How it works:** When node 10,001 is ingested, the daemon triggers a background sweep. It identifies the oldest chronological node in the HNSW graph and deletes its vector and edges from the in-memory structure.
*   **RAM Reclamation:** Go's GC immediately reclaims the 4 KB of memory. 
*   **The Resulting Curve:** 
    *   *Months 1-3:* RAM slowly climbs from 20 MB to ~45 MB.
    *   *Month 4+ (Infinity):* The RAM curve completely flattens. The daemon will run for 10 consecutive years without ever crossing the 50 MB threshold. The user retains hyper-fast semantic search for the last ~3 months of their life, while older memories are safely archived on disk.

---

## 6. Backlog — User Stories

### US-1: Launch ORA automatically on login

> **As a** user, **I want** ORA to start automatically when I log in, **so that** the
> ambient companion and activity tracker are always running without me launching it
> by hand.

Because ORA is a background daemon, "always on" is the expected behaviour — manual
launch defeats the ambient-memory premise. The TUI client is launched on demand; this
story is specifically about the **daemon** auto-starting at login (the client can keep
its get-or-create-daemon spawn logic).

**Acceptance criteria**
- A first-run/`--install-autostart` step registers the daemon to start on login.
- `--uninstall-autostart` cleanly removes it. Idempotent; never double-registers.
- No elevated privileges required (per-user autostart, not system service).
- Pure-Go, no CGO; no external installer framework.
- Survives a binary move/update (store the resolved absolute exe path, or re-resolve).

**Per-OS approach (pure-Go friendly)**
- **Windows (.exe):** simplest is the per-user **Run** registry key
  `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` → value `Ora` = `"<exe> --daemon"`
  (via `golang.org/x/sys/windows/registry`, no CGO). Alternative: drop a shortcut in the
  Startup folder, or register a Task Scheduler task (`schtasks`) for "at logon" with a
  hidden window. Run key is the least-friction default.
- **Linux:** write a **systemd user unit** to `~/.config/systemd/user/ora.service`
  (`ExecStart=<exe> --daemon`, `WantedBy=default.target`) and `systemctl --user enable`.
  Fallback for non-systemd / minimal sessions: an XDG autostart desktop entry at
  `~/.config/autostart/ora.desktop`. Both are plain text files — trivial to write/remove.
- **macOS (deferred):** a LaunchAgent plist in `~/Library/LaunchAgents/`.

**Open questions**
- Opt-in on first run vs. an explicit `ora --install-autostart` command? (Lean: prompt
  once in the TUI, plus the explicit flags for scripting.)
- Should autostart launch the daemon only, or also the TUI? (Lean: daemon only.)
