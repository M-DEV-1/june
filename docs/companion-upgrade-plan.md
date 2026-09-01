# ORA Companion Upgrade — Plan

> ORA should feel like a **personal secretary and friend**, not a surveillance log that
> reads back "here's what this person did." This document is the north star for that shift.

## The thesis: a mind has two faculties

A companion that feels alive needs both halves of memory:

1. **Pattern recognition** — *"have I seen this before? is this new, or the same?"*
  This is what makes the everyday feel textured. We notice novelty, recognize routine, and feel the difference between them. ORA today has only a crude binary version of this:  `IsSalient()` (`internal/memory/compiler.go:250`) drops obvious noise and keeps everything else with equal weight. It cannot tell that you're *back* on the ESG report, or that this meeting is *new*.
2. **Retrieval** — *"do I recall that this happened?"*
  This is memory proper. ORA today either dumps **everything** (all notes, unconditionally, `db.go:599`) or finds **nothing** (FTS5 keyword search reachable only if the model chooses to call the `query_memory` tool, `db.go:342`). Neither is recall.

Every complaint the user raised traces back to a weakness in one of these two faculties, or to the persona that sits on top of them. The plan below is organized accordingly.

---

## Diagnosis → root cause map


| Symptom (user's words)                                        | Root cause                                                                                                                                   | Location                                                    |
| ------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------- |
| "feels like a robot reading out what I did"                   | persona framed as a *"Temporal Brain"* observability tool; opening turn *orders* it to "acknowledge what I'm working on"                     | `connect.go:86`, `connect.go:120`                           |
| "I ask what series I'm watching — it doesn't know"            | working-state early-return injects notes + one `[state]` blurb and **never retrieves** episodic memory; FTS5 is keyword-only and model-gated | `db.go:609-614`, `db.go:342`                                |
| "I ask what I'm doing — it cites privacy"                     | prompt primes caution ("private memory", "without being intrusive"); empty/thin context → canned deflection                                  | `connect.go:86`                                             |
| "ESG facts bleed into an unrelated code project"              | **all notes dumped into the prompt unconditionally**, zero relevance filtering                                                               | `db.go:599-604`                                             |
| "reads another codebase, asks 10 times what to look at"       | thin tools (`read_file` 4000-char cap, no explore loop) + injected memory pulling attention to unrelated facts                               | `tools.go`, `db.go:599`                                     |
| "voice breaks in longer conversations; buffers aren't enough" | not buffer size. Likely `writeMu` serializing all sends + Gemini Live session lifetime/lossy reconnect                                       | `connect.go:114/242/278`, `client.go` (under investigation) |
| "I want it to know my meetings and ask how they went"         | no proactivity mechanism at all; 100% reactive; no calendar ingestion                                                                        | new work                                                    |


---



## Workstream A — Persona (STARTING NOW)

Pure prompt + interaction shaping. Biggest immediate tone win; no architecture change.

- **Rewrite the system instruction** (`connect.go:86`): from "Temporal Brain / ambient AI agent maintaining high-fidelity understanding of the workspace" → a warm personal secretary-and-friend who has been quietly alongside the user all day and *remembers*.
- **Kill the privacy deflection**: explicitly tell it this is the user's own companion on the user's own machine; questions about the user's own activity are exactly what the memory is for — answer them directly, never deflect with privacy.
- **Rewrite the opening turn** (`connect.go:120`): from "acknowledge what I'm working on right now" (forces telemetry read-back) → a natural, warm greeting that references the day only when it feels human.
- **Personality + proactive warmth**: it may follow up on things it noticed ("how'd that meeting go?") naturally, not as a report.

*Note:* persona alone reduces robot-feel and deflection a lot, but the **ESG bleed and the "doesn't know my series" recall gap are fixed in Workstream B**, not here.

## Workstream B — Memory model: recognition + retrieval

This is the spine. Two sub-parts mirroring the two faculties.

### B1 — Pattern recognition (novelty/familiarity)

- Replace binary `IsSalient()` with a **salience score + novelty flag**. The `nodes` schema already reserves a TODO for a salience score.
- A lightweight **recognition stage** in the compiler pipeline: when an activity arrives, ask "is this familiar (seen recently/ever) or novel?" v1 heuristics — match against recent activity + known task names; later, semantic similarity.
- Outputs feed two things: (a) higher salience for novel events so they're remembered well, (b) a signal ORA can *speak* from — "you're back on X", "this looks new."



### B2 — Retrieval (relevance-filtered FTS5 first; vectors later)

- **Stop the blind dump.** Replace unconditional note injection in `GetImplicitContext` (`db.go:599`) with relevance-ranked retrieval: derive query terms from current activity → pull only notes/summaries relevant to the moment → inject top-k.
- **Always retrieve episodic memory**, not just notes/state — so "what series am I watching" surfaces from summaries/digests.
- Define a `Retriever` **interface** so FTS5 backs it now and embeddings + HNSW (`EmbedModel` already defined in `config/gemini.go:12`) swap in later without touching call sites. *(User decision: FTS5 first, vectors behind same interface later.)*
- Consider a mid-session retrieval hook (topic-shift → push relevant memory) since the Live API system prompt is set once at connect.

## Workstream C — Audio stability

**Root cause found (investigation complete).** "Buffer flooding" is a *secondary symptom*,
which is why bumping buffers never helped — the underlying stalls are unbounded, not spikes.

**Primary cause — Gemini Live session expiry + lossy cold reconnect.** The Live API ends a
native-audio session after ~15 min (or earlier once audio tokens fill the context). The server
sends a `GoAway` message — which `receiveLoop` (`connect.go:151`) **never checks** — then closes
with 1011. The reconnect loop (`client.go`) comes back as a **cold start**: `Connect()` builds a
fresh `LiveConnectConfig` with no resumption and re-fires the greeting, so all conversation
history is lost. This is exactly "voice breaks in longer conversations." The config at
`connect.go:71` sets neither `SessionResumption` nor `ContextWindowCompression`, both of which
the genai SDK (v1.56.0) exposes.

**Secondary cause —** `executeTool` **runs synchronously inside** `receiveLoop` (`connect.go:230`;
HITL `shell_exec` blocks at `tools.go:137` indefinitely). While blocked, nothing calls
`session.Receive()`, so server pings go unanswered and the TCP receive buffer backs up →
backpressure stalls the write path → `audioSendLoop` blocks on `writeMu` → `micChan` (100 slots,
~5 s) overflows and drops frames. *That* is the "flooding."

### Fix order (confidence-ranked)

1. `ContextWindowCompression` in `LiveConnectConfig` (~5 lines) — server trims oldest turns;
  session never exhausts context. Single highest-leverage fix.
2. **Handle** `GoAway` in `receiveLoop` (~10 lines) — make expiry observable + warn the user.
3. `SessionResumption` — store the resumption handle from `SessionResumptionUpdate`, pass it
  on reconnect; gate the greeting so it only fires on a cold start. Makes reconnects stateful.
4. **Async tool execution** — move `executeTool` to its own goroutine + a `toolSendLoop`, so the
  receive path never freezes during tool calls. Independently kills the mic-drop symptom.
5. **Log** `micChan` **drops** (`capture_linux.go:writeFn`) — cheap observability to confirm #4.



## Workstream D — Proactivity + calendar (the vision)

Builds on A + B. Three new capabilities:

1. **Trigger/event system** in the daemon: return-after-absence, calendar-event-ended,
  long-focus-block-ended.
2. **Unprompted turn injection** into the live session so ORA can initiate ("how'd the
  meeting go?").
3. **Calendar ingestion** as a first-class memory source feeding both recognition and
  retrieval.

---



## Sequencing

1. **A — Persona** (now): immediate, visible, low-risk.
2. **B2 — Relevance retrieval** (next): fixes the bleed + recall; highest functional payoff.
3. **B1 — Recognition/novelty**: makes the everyday feel textured.
4. **C — Audio**: foundational stability; gated on the diagnostic.
5. **D — Proactivity + calendar**: the secretary-and-friend vision, once memory is trustworthy.

PRs can be split per workstream even though they share the `feature/companion-upgrade` branch.