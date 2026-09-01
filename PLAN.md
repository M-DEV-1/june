# ORA Memory Architecture Plan

**This plan should be stored as `PLAN.md` at the project root.**

See below for full details.

---

# Detailed Plan: Rethink ORA Memory Architecture + Introduce Vector Search

## Context
The capture harness (`internal/tracker/`) is solid: it produces rich `Activity{App, Title, ScreenText}` (AT-SPI tree walks + vision tier fallback for thin text) with implicit timing. However, the memory architecture immediately abstracts this into lossy high-level `TaskSummary` (short `task_name` + 1-2 sentence `summary` + restricted `notes`) stored as JSON under a temporal tree (`USER→DAY→SESSION→TASK→summary`).

**Current problems (from user feedback + code analysis)**:
- Rich `ScreenText` (game states like "Riddler puzzles", specific UI/docs content, terminal output, vision descriptions) is fed to LLM summarizer in `compiler.go` but **never persisted raw**. Only condensed version survives.
- `LogActivity` stores only `"app | title"` (thin); `CullRawActivities` drops even that after 72h.
- Retrieval (`SearchMemory` FTS5, `QueryMemory` LIKE on JSON, `GetImplicitContext`, `RetrieveRelevant`) operates on abstractions → keyword-only, no semantics, poor temporal narratives ("your morning started with...").
- "What was I doing yesterday?" fails to reconstruct flow; model falls back to current `working_state` + vague inference. FTS often returns "no memory matches".
- Recent relevance work (focusSignal + `RetrieveRelevant` using FTS5) helps bleed but is still lexical on poor data.
- Result: log-like summaries, not companion-like recall of specifics/experience. Matches symptoms in `docs/companion-upgrade-plan.md` (e.g., "doesn't know my series").

**User goals**: Preserve/use harness data for meaningful recall (temporal stories, specific on-screen details like game actions). Dual-layer thinking: rich observations (episodes) as primary recent substrate + derived summaries for synthesis/long-term.
- Introduce vector search as part of rethink for **semantic** retrieval over rich content (not just FTS5 keywords).
- Question: vector DB choice? Way forward? Expected response quality post-changes?

**Existing vision** (do not reinvent):
- `RESEARCH.md`: Recommends **pure-Go `github.com/coder/hnsw`** (SavedGraph, cosine) + Gemini embeddings (async post-`LogSemanticNode`); store vectors as BLOB or sidecar; **hybrid** (vector first + FTS5/LIKE fallback); temporal pruning (cap ~10k nodes); augments (does not replace) the tree.
- `docs/companion-upgrade-plan.md`: FTS5/relevance first (done); vectors later behind `Retriever` interface (`EmbedModel` in config already).
- `internal/config/gemini.go`: `EmbedModel = "gemini-embedding-2"` (SDK supports `EmbedContent`).
- `go.mod`: `google.golang.org/genai`, `modernc.org/sqlite` (pure-Go, no CGO/extensions). `Retriever` interface + comment in `db.go` ready for vector impl ("vector stores can implement later").
- No current vector code (hnsw dep absent; no `EmbedContent` calls; FTS5 only).

This plan sequences: (1) storage fix (preserve rich data), (2) vector/hybrid on top for semantics, (3) use for better narratives/context. Aligns with TDD (user preference), worktree isolation, no-commits.

## Recommended Approach
**Phase the rethink**:
1. **Storage layer first (fundamental)**: Add "episodes" (or 'episode' nodes) as first-class recent episodic units. Preserve timestamp + rich `ScreenText` (capped/excerpted for size) + title/app from capture. Link to temporal tree. This gives "raw material" for everything downstream. Keep current summaries/threads/working_state/notes as **derived** (LLM labels/synthesis/compaction from episodes).
2. **Retrieval evolution**: Extend `RetrieveRelevant`/`SearchMemory` (or hybrid) to surface episodes. Add time-ordered walks for narratives.
3. **Vector search**: Add as semantic layer **on the richer data**. Use `coder/hnsw` (pure-Go, as per RESEARCH; no full "vector DB" like Weaviate to stay thin/self-contained). Embed episodes + summaries. Hybrid FTS5 + cosine similarity. Async embedding via genai (leverage existing client).
4. **Usage/meaningful output**: Update `GetImplicitContext`, agent tools, prompt injection, compiler to use episodes for context + on-demand narrative synthesis. This enables expected behaviors (see below).
5. **LLM role**: Still condenses for working_state/digests/threads (keep compact), but on-demand for stories from episodes. Reduces early loss.

**Vector DB choice**:
- **Do not use** a full vector DB/server (Weaviate, pgvector, Chroma — rejected in RESEARCH for deps, bloat, CGO conflict with modernc/sqlite).
- **Use**: `github.com/coder/hnsw` for the index (`SavedGraph[string]`, key = node ID; persists to `ora-db/vectors.hnsw` or `graph.bin`; `Add`/`Search` with `CosineDistance`).
  - Vectors stored as BLOB in SQLite `nodes` (add `embedding` column or side table) or alongside (per RESEARCH).
  - Pure-Go, zero CGO, tiny footprint at ORA scale (~3-4KB/node).
  - Hybrid with existing FTS5 (vector for semantics + FTS for keywords/exact; fallback always available).
- Embeddings: Reuse `genai` client + `EmbedModel` ("gemini-embedding-2"; ~768d `[]float32`). Call async post-write (never sync path). Free tier ample.
- Alternative (if hnsw proves low-level): `github.com/philippgille/chromem-go` (higher-level pure-Go docs), but prefer hnsw per RESEARCH.
- Fallback/offline: Keep FTS5; future opt-in local ONNX possible but deprioritized (breaks thin binary).
- Why now: Complements rich episodes (semantic on actual screen text). `Retriever` interface makes swap clean. Start with recent data (prune old from HNSW).

**Go-forward sequence** (TDD, worktree):
- Explore/plan (this).
- Storage: Extend `Activity` (add `Timestamp`), add episode persistence in `db.Store` (use/extend `nodes` with `type='episode'` for tree compatibility + rich `content` or new fields; FTS5 trigger for episode content).
- Compiler/daemon: In `flush`/`Ingest` + event loop, persist episodes (full/capped `ScreenText`) *before* heavy condensation. Derive summaries from episodes.
- Vectors: Add dep; extend `Store` (load `hnsw.SavedGraph`); async embed on episode/summary write; update `SearchMemory`/`RetrieveRelevant` to hybrid (embed query → hnsw.Search → load + rank with FTS).
- Retrieval/use: Make `GetImplicitContext`/`query_memory` tool use episodes + vectors for relevance. Add helpers for chronological narrative from episodes (LLM polish on-demand).
- Compaction/pruning: Extend for episodes (keep recent rich; age screen_text; HNSW cap ~10k).
- Persona/tests: Leverage existing (no unconditional dumps).
- Build/iterate in `ora-relevance` worktree (copy .env, `go build`, run daemon + TUI for observation).

**Tradeoffs**:
- Storage: Episodes add size (cap screen_text ~1-10KB; age policy). Mitigate with WAL + pruning.
- Complexity: Hybrid retrieval + async embeds. Mitigate with interfaces + phased (FTS first, vector add-on).
- Scale: HNSW + cap keeps <50MB RAM (per RESEARCH calcs).
- Migration: Additive (new node types/columns + hnsw file). Old data stays summarized.
- LLM quality: Still depends on Gemini for condensation/embed; rich input improves output.

This keeps screen specifics (app, title, visible content) while preserving tree/FTS/relevance benefits.

## Critical Files to Modify
- `internal/tracker/tracker.go` (add `Timestamp time.Time` to `Activity`; update `Normalize`).
- `internal/tracker/daemon.go` (pass timestamp on emit; ensure `ScreenText` flows).
- `internal/db/db.go` (extend schema for episode support + optional embedding BLOB; implement `LogEpisode` + hybrid `SearchMemory`/`RetrieveRelevant`; update `Store` with hnsw graph; triggers for FTS on episodes).
- `internal/memory/compiler.go` (in `flush`: persist episodes from buffer; call `LogSemanticNode` as derived).
- `cmd/daemon.go` (wire episode logging in event loop; async embed goroutine hook; HNSW init).
- `internal/config/gemini.go` (already good; possibly tune).
- `go.mod` (add `github.com/coder/hnsw v0.6.1`).
- `internal/agent/agent.go` + `tools.go` + `connect.go` (expose episode/narrative via `ContextReader`; update tool descs + context injection for richer recall).
- `internal/memory/state.go` / `compaction.go` (extend for episodes where needed).
- Tests: `internal/db/db_test.go`, `internal/memory/compiler_test.go` (new episode + semantic tests).
- Docs: Update `RESEARCH.md`, `docs/companion-upgrade-plan.md` if needed; reference `ARCHITECTURE_RETHINK.md` (worktree).

Work primarily in `/home/mdev1/Desktop/Code/projects/ora-worktrees/ora-relevance` (as prior sessions; `cp .env`, build/test there).

## Existing Functions/Utilities to Reuse (with Paths)
- `tracker.Activity` + capture flow (`internal/tracker/tracker.go:8`, `internal/tracker/daemon.go:147` (tiered `capture`/`extractText` + vision), `internal/tracker/capture_linux.go` (AT-SPI), `internal/tracker/screenshot_linux.go` (grab)).
- `GeminiSummarizer` methods (`internal/memory/compiler.go:98` Summarize/AttributeThreads; reuse `client` for `EmbedContent` later; `DescribeScreen:305`).
- `LogSemanticNode` + tree helpers (`internal/db/db.go:901`, `ensureNode`, `createSchema`).
- `SearchMemory` (FTS5 base; `db.go:371` — extend to hybrid).
- `GetImplicitContext` (`db.go:684` — evolve to include episodes).
- `RetrieveRelevant` / `Retriever` iface (planned in upgrade-plan; comment in `db.go:341`; implement vector path).
- `QueryMemory` (legacy LIKE fallback; `db.go:965`).
- `Compiler.Ingest`/`flush` + `IsSalient` (`internal/memory/compiler.go:388` etc.; preserve buffer for episodes).
- `Store` struct + mu (`internal/db/db.go:26`).
- GenAI client wiring (`internal/config/gemini.go:10`, used in `compiler.go` + `connect.go`).
- HNSW patterns from RESEARCH (SavedGraph, async embed post-Log*, hybrid in Query/Search, pruning).
- Existing relevance (focusSignal in GetImplicitContext; dynamic recall in `internal/agent/connect.go`).
- Compaction (`internal/memory/compaction.go:58` `Digest`; extend for episodes).
- TDD patterns (tests in `db_test.go`, `compiler_test.go` using `LogSemanticNode` + FTS assertions).

Add `hnsw` dep + reuse genai `EmbedContent` (SDK supports; example in RESEARCH).

## Verification (End-to-End Test)
1. **Unit/TDD**:
   - Add tests: capture rich `Activity` with ScreenText (e.g., "Riddler puzzle on screen") → `LogEpisode` persists with ts + text → FTS + vector search finds semantically similar ("game UI state") or keyword.
   - Timeline: episodes under day node ordered by ts → reconstruct "morning... afternoon..." narrative (via helper + LLM polish).
   - `RetrieveRelevant` with focus returns episode excerpts (not just summaries).
   - Run `go test ./internal/db ./internal/memory -run 'Episode|Vector|Recall'` (in worktree).
   - Mock hnsw/embed for offline tests; real Gemini in integration.

2. **Integration**:
   - In worktree: `cp ../../ora/.env .env; go build .`
   - Run daemon + TUI; simulate day (browse, code, "game" via terminal titles + text, Excel/Teams).
   - Trigger flush → verify episode nodes (use `sqlite` or `store.DB()` queries in test harness) contain ScreenText excerpts + ts.
   - Use `/context`, `query_memory` tool, or direct "what was I doing yesterday?" / "recall when I saw Riddler".
   - Check logs for embeddings (async), hybrid results, richer context injection.
   - Observe no breakage: existing FTS, summaries, working_state, threads, compaction still work.

3. **End-to-end / Behavioral**:
   - After changes + hours of daemon: Ask temporal ("walk me through yesterday"), specific ("what game detail did I see?"), semantic ("things like the audio debugging").
   - Verify: Responses include ordered flow ("started with X while screen showed Y, then..."), specifics from capture (not hallucinated), relevant episodes surfaced via vector (e.g., similar UI states), using focus + hybrid.
   - Metrics (manual): Less "no memory matches", richer non-bleedy recall, companion feel ("I remember you were...").
   - Edge: Cold start (load hnsw), pruning, thin vs rich screens, old data fallback.
   - Build/run: `go build .; ./ora` (daemon mode); TUI interactions.

4. **Regression**: All existing `db_test.go` / compiler tests pass. No CGO. Pure-Go constraints. Run full `go test ./...`.

5. **Observation**: Use real `ora-db/db` + logs post-run. Compare pre/post vector: semantic matches improve.

## Expected Response Quality Post-Changes ("what can I expect my responses to be like?")
- **Temporal narratives**: "Your morning started with terminal work on the ORA compiler (screen showed Go code for flush logic), then you switched to browsing ESG reports while in a Teams standup (visible: energy sector data), afternoon deep in the 16-model OCR eval spreadsheet where GPT-5 Mini outperformed... Later you were playing around in a game title with Riddler elements on screen."
- **Specifics from harness**: Direct recall of screen content ("when you were looking at that exact function in auth.go...") or game states via semantic/keyword on preserved episodes.
- **Semantic + relevance**: "Things like the audio debugging" surfaces conceptually similar past episodes (even paraphrased), injected in context or via tool — less "no matches", more "I recall you were..."
- **Companion-like, not log**: Natural weave-in (via updated persona + rich context); proactive ("how'd that Riddler section go?"); no bleed (relevance + episodes scoped).
- **Better than before**: From condensed task labels to experience replay. Still uses summaries for high-level overview/working state. Model can ask for more detail via tools.
- Limits: Still LLM-dependent (quality varies); recent only for full richness (old = summaries + fallback); vector adds semantics but not perfect (hybrid helps).

This delivers on "it knew" use case while staying thin/pure-Go.

## Open Items / Risks (for user input if needed)
- Exact episode schema (nodes extension vs new table? cap on screen_text?).
- Async embed error handling / offline mode.
- HNSW version/pin + persistence details (follow RESEARCH).
- Performance at 10k nodes (bench in plan verification).
- If ambiguity: Ask user on vector vs pure FTS priority or episode granularity.

Execute via TDD in worktree after approval. Start with storage slice.