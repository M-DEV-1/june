# ORA — Known Bugs (verified against code, 2026-07-24)

**Update, same day**: P0 items 1-4 below are fixed and TDD-verified (`go build ./...`,
`go vet ./...`, `go test ./internal/... -count=1` all green; `evals/` untouched; no
commits). An adversarial review pass then caught that fixing #1 required weakening a
pre-existing regression test in a way that papered over a second, related bug — see
"P0.5" below, also now fixed. Kept the original entries as-written (struck through)
so the diagnosis trail stays intact.

**Update, 2026-07-25**: P1 (audio/session stability, all 4 items) and P4
(episode row-count cap, notes dedup) are now also fixed and TDD-verified —
two background agents implemented each independently, then an adversarial
review pass ran against both diffs plus `go test ./internal/agent/...
./internal/db/... -race -count=1` (neither agent had run `-race`
themselves). Verdict: no races, no leaks beyond one deliberately-accepted
goroutine-leak edge case (see P1 §4), correct SQL/trigger reasoning for
`PruneAncientEpisodes`, and no *new* weakened tests — the review's two
test-weakening flags both traced back to the pre-existing P0.5 regression
fix (`recentTaskWindow`), already documented and fixed above, not to any
new P1/P4 work. Full repo (`go build`, `go vet`, `go test ./...`) green.
Still no commits.

Single source of truth for open bugs. Supersedes scattered mentions in
`RESEARCH.md` / `PLAN.md` / `IMPROVEMENTS.md` — those are prior brainstorm,
not gospel; this file is what's actually confirmed against the live code
right now. Each entry is marked **VERIFIED** (read the code, confirmed) or
**REPORTED** (surfaced from past user sessions, not yet checked against code).

No commits. This is a working list to fix against, not an archive.

---

## P0 — Retrieval / memory-tool bugs — FIXED

### 1. FTS5 whole-query phrase-quoting kills recall — FIXED
`internal/db/db.go`:
- `SearchMemory` (~line 438): `safe := `"` + strings.ReplaceAll(query, `"`, `""`) + `"`` wraps the **entire** query as one FTS5 phrase.
- `SearchEpisodes` (~line 1317): identical pattern.
- `RecallSubject` (~line 1276): calls `SearchMemory` directly, inherits the same bug.

MATCH wants tokens; a whole-string phrase only matches content containing that
exact contiguous run of words. Natural-language questions almost never share
a verbatim substring with stored prose. This is confirmed by a real recall@5
= 0.000/17 failure already reproduced as a test in this repo (not our
concern to run/fix that test itself — the point is the same query-builder
bug hits `query_memory`, `recall(subject=...)`, and episode search).

**Fix direction:** tokenize the query, drop stopwords, build an OR-of-terms
FTS5 MATCH string (`term1 OR term2 OR term3`), reserve phrase-quoting for
explicit multi-word quotes the user/model actually asks for. Apply via one
shared helper, not three copies.

### 2. Unbounded note dump moved, didn't disappear — FIXED
`cmd/daemon.go` (~line 169): `notes, _ := store.GetNotes(ctx)` returns **all**
notes, uncapped, fed straight into `DeriveState` (the working-state LLM call)
every 5 minutes. `GetImplicitContext` was already fixed to stop doing this;
this call site wasn't.

**Fix direction:** same relevance-filter/cap treatment `GetImplicitContext`
got — top-k relevant notes, not the whole table.

### 3. Context-injection turn doubles every Live API round-trip — FIXED
`internal/agent/connect.go:365-366` (`textSendLoop`): sends `turnContext`
(the now-anchor + recall payload) as its **own** `SendClientContent` call,
immediately followed by a second call with the real user text. Neither sets
`TurnComplete`. Checked the SDK source
(`google.golang.org/genai@v1.56.0/live.go:197-199`): `TurnComplete` **defaults
to `true`** when omitted. So both calls are complete turns — the model can
reply to the bare "[context] It is now Monday..." line as if it were user
speech, and every real user utterance costs 2 Live-API turns instead of 1.
This directly compounds the long-conversation degradation already diagnosed
in `docs/companion-upgrade-plan.md` Workstream C (session/context exhaustion,
history growth).

**Fix direction:** merge the context string and the user text into one
`Content`/one `SendClientContent` call (e.g. prepend `[context] ...` as a
second `Part` in the same turn, or concatenate into one `Part`).

### 4. `recall` tool: no since/until ordering check, silent type coercion — FIXED
`internal/agent/tools.go`:
- `recallBounds` never checks `since <= until`. A reversed range returns "no
  episodes in that window" — indistinguishable from a genuinely empty day.
- `sinceStr, _ := args["since"].(string)` (and the same for `untilStr`)
  discards the ok-bool. A malformed non-string arg (e.g. the model sends a
  JSON number) silently becomes `""` and falls through to the default,
  instead of surfacing an error the model could correct on the next turn.

**Fix direction:** validate `since <= until` and return an explicit error
string when violated (mirrors the existing parse-error return path); use the
ok-bool from the type assertion and error out on a present-but-wrong-typed
arg rather than silently defaulting.

### 4.5. Reversed-range error message misleadingly claimed a format problem — FIXED
Adversarial review nit: the reversed-`since`/`until` error reused the generic
"since/until must be ISO-8601 timestamps" prefix, misleading for an ordering
problem, not a format problem. Fixed with a sentinel error
(`errSinceAfterUntil` in `internal/agent/tools.go`) so the two failure modes
get distinct messages. Test: `TestExecuteTool_Recall_ReversedRangeErrors`
tightened to assert the message does NOT contain "ISO-8601" and DOES state
the real ordering problem.

### P0.5. Fixing #1 exposed a second bleed vector: unbounded task-name recency — FIXED
Adversarial review of fix #1 caught this: `GetImplicitContext` (`internal/db/db.go`)
folds the last 2 task names into its relevance focus signal by **id**, with no
time bound. Before fix #1, this was mostly inert (whole-phrase quoting rarely
matched anything). After fix #1's OR-of-terms tokenizer, a task's own name in
the focus signal makes it trivially self-match its own summary — and because
there was no recency bound, a *stale* task (days old) sitting in "last 2 by
id" in a lightly-used store could keep bleeding into unrelated contexts
forever. This is the same shape as the original unconditional-notes-dump bug,
just relocated. A first-pass fix for #1 papered over this by weakening
`TestStore_GetImplicitContext_WithWorkingState`'s regression guard instead of
addressing it — caught on adversarial review, not shipped as-is.

**Fixed**: `internal/db/db.go` now bounds the task-name query to a
`recentTaskWindow` (2h) via `created_at >=`, so stale tasks drop out of the
focus signal instead of self-matching indefinitely. New regression test:
`TestStore_GetImplicitContext_DoesNotLeakStaleTaskAcrossContexts` — reproduces
the project's own "ESG bleed" scenario directly (an old, backdated, unrelated
task's summary must not resurface once a fresh working state is set),
verified red against the pre-fix query, green after.

### 5. Episodes have no day/session link — VERIFIED, lower urgency
`internal/db/db.go` `episodes` table (~line 199) has no `day_id`/`session_id`
column. Pre-episode-era days (before 2026-07-05) and any future
day-tree-structural walk can't join episodes to a day node. `EpisodesInWindow`
currently works around this by querying `created_at` directly, so this isn't
blocking the recall tool today — only a real gap for anything that wants to
walk the `nodes` tree and pick up episodes along the way.

---

## P1 — Audio / long-conversation stability — FIXED

All four implemented in `internal/agent/connect.go` (+ a small `resumeHandle`
field/accessors on `Agent` in `agent.go`). Independently verified: `go build
./...`, `go vet ./...`, full test suite, and `go test ./internal/agent/...
-race -count=1` all clean; adversarial review found no concurrency bugs and
no weakened tests in this diff.

1. **`ContextWindowCompression`** — set in `LiveConnectConfig`
   (`TriggerTokens: 25000`, `SlidingWindow.TargetTokens: 8000`, the commonly
   cited Live API defaults). Static config, verified against the SDK's real
   field/type names; exercised indirectly by the existing
   `TestAgent_ConnectFailsWithBadKey` (proves the config marshals and is
   accepted before failing on the bad key).
2. **`GoAway`** — logged (non-fatal, `slog.Warn`) in `receiveLoop` right
   after `Receive()` succeeds; loop continues normally so any other field on
   the same message (e.g. a co-arriving `SessionResumptionUpdate`) still
   gets processed.
3. **`SessionResumption`** — `receiveLoop` captures
   `msg.SessionResumptionUpdate` into `Agent.resumeHandle` (only when
   `Resumable == true` and `NewHandle != ""`, so an unresumable update never
   poisons the next reconnect into wrongly skipping the greeting).
   `Connect()` reads the stored handle, always requests resumption via
   `SessionResumptionConfig{Handle: priorHandle}`, and gates the "just came
   online" greeting on `priorHandle == ""` so a resumed session isn't
   talked over.
4. **Async tool execution** — `executeTool` no longer runs inline in
   `receiveLoop`. A new `liveSession` interface (`Receive`/
   `SendToolResponse`) lets `receiveLoop` take either the real
   `*genai.Session` or a fake; each tool call is dispatched via
   `go a.runToolCall(...)` and the loop immediately goes back to
   `Receive()`. Proven by `TestReceiveLoop_ToolCallDoesNotBlockReceivePath`
   in `internal/agent/connect_test.go`, which drives a **real** blocking
   HITL `shell_exec` call followed by a fast call and asserts the fast
   call's response arrives before the slow call's approval is even
   resolved — this test genuinely hangs under the old synchronous code
   (verified by reasoning through the old path, not just asserted).
   `a.writeMu` still serializes every write to the session
   (`runToolCall`/`audioSendLoop`/`textSendLoop` all lock around `Send*`),
   so concurrent tool responses can't interleave with audio/text sends.

**Known, accepted gap (not a bug, flagged deliberately):** a tool-call
goroutine blocked on a HITL approval that never arrives leaks until (if
ever) approved, if the session disconnects/reconnects first. `SendToolResponse`
on a stale session returns a plain error (logged), not a panic, so this is
not a crash risk — just unbounded-in-theory goroutine accumulation under
heavy, unapproved-HITL usage. Building full in-flight-call
cancellation/teardown was judged out of scope (this is strictly better than
the prior state, where a single stuck approval hung the entire receive
loop). Revisit if HITL timeouts become common in practice.

---

## P2 — Leads from past sessions, not yet root-caused

- **REPORTED**: meetings, Discord, and gaming sessions sometimes aren't
  captured at all — user suspects "some decision junction" filters activity
  before it reaches storage. This is upstream of everything above (capture
  path, not retrieval). Needs its own investigation of `IsSalient()` /
  blocklist logic in `internal/memory/compiler.go` / `internal/tracker/`.
- **REPORTED**: possible failed-tool-call rate issue — some bad answers may
  be silently-failed tool calls rather than genuinely bad retrieval. No code
  evidence yet either way.
- **REPORTED**: user wants scene-level narrative recall and associative
  graph-linking between related episodes (not just a flat notes/timeline
  model) — references HippoRAG's graph-walk as inspiration. This is an
  architecture direction, not a bug — tracked here so it doesn't get lost,
  see the combined-retrieval design doc for how it factors in.
- **REPORTED**: compaction-vs-embedding tension — `AgeEpisodes` clears
  `screen_text` on old low-importance episodes; user is concerned this
  destroys raw text future embeddings would need. Needs a decision before
  any aging-policy changes ship alongside vector search.

---

## P3 — Known bug *classes* from comparable tools (check ora for analogues, not yet confirmed present)

From mem0 / Letta / llama_index issue trackers — patterns worth a quick check
against ora's own tool implementations, not confirmed bugs in ora itself:

- **Stale client after idle** (mem0 #2672): a long-lived client/session
  returning empty/stale results until reinstantiated. Check whether ora's
  `*db.Store` or genai client has any equivalent staleness window.
- **Silent filter-combination footgun** (llama_index #20492): passing two
  filter args together (e.g. `user_id` + `agent_id`) silently ANDs to an
  empty result instead of erroring. Check `query_memory`/`recall`'s argument
  combinations for the same shape.
- **Tool-call arg confusion between similarly-named memory tools** (mem0):
  model confuses which tool needs which arg (e.g. `memory_id` on the wrong
  call). Worth a schema-conformance check on ora's tool definitions.
- **Context budget exceeded on smaller models** (Letta #957): less relevant
  since ora is Gemini-only, but is a reminder that context-injection size
  needs its own hard cap rather than trusting the model to self-truncate.

---

## Dependency risk to flag for whenever vector search is implemented

`github.com/coder/hnsw` (the pure-Go, no-CGO HNSW candidate) has **zero
internal synchronization** (`Add`/`Search`/`Delete` mutate shared state with
no lock) and an **open, unresolved upstream issue**
([coder/hnsw#15](https://github.com/coder/hnsw/issues/15)) reproducing a
panic under concurrent add+search even with an external mutex added by the
reporter. Any design that embeds-and-inserts from a background goroutine
while a query path searches concurrently needs at minimum a coarse
`sync.RWMutex` around the whole graph, and should budget time to hit that
open panic class during integration. Not a reason to avoid the library — just
not the "drop-in safe" library the earlier docs assumed.

**Decision, 2026-07-24**: use chromem-go now (see the memory-architecture
deck, `docs/ora-memory-deck.html`, §03). `coder/hnsw#15` is a real, doable fix
(the panic traces to the "elevator" layer-descent pointer not being
invalidated on a duplicate-key re-add) and worth attempting eventually as an
upstream contribution — but it unblocks nothing for ora today, since the
whole point of picking chromem-go was to sidestep this exact bug. Track as a
someday/backlog item, not a dependency of shipping the hybrid harness.

## P4 — Multi-year growth: what's bounded, what isn't (raised 2026-07-24)

Verified against the current daemon/db code, not speculation:

- **Summaries**: bounded. `Compactor.Compact` (12h ticker, `cmd/daemon.go`)
  rolls summaries older than 7 days into a digest and deletes the originals.
- **Episode raw text**: bounded. `AgeEpisodes` (24h ticker, confirmed wired at
  `cmd/daemon.go:~226`) clears `screen_text` after 10 days for episodes below
  a 0.3 importance floor.
- **Episode row count**: **FIXED**. New `Store.PruneAncientEpisodes(ctx,
  olderThan)` (`internal/db/db.go`) deletes episode rows older than a
  threshold, but *only* if `screen_text` is already empty (i.e. only rows
  `AgeEpisodes` has already thinned) — a row still carrying raw text is
  never touched here regardless of age, so this cannot destroy un-aged
  data. Wired into `cmd/daemon.go` as a weekly ticker
  (`ancientAfter = 365 * 24h`), independent of and non-conflicting with the
  existing 24h `AgeEpisodes` ticker. `episodes_fts` stays in sync via the
  pre-existing `episodes_ad AFTER DELETE` trigger — no new trigger needed;
  confirmed correct by an FTS5 `integrity-check` command in the new test
  (`TestStore_PruneAncientEpisodes_DeletesOnlyThinnedRowsPastThreshold`),
  which is the definitive proof the shadow index wasn't orphaned, not just
  an absence-of-search-hits check.
- **Notes**: **FIXED**. `LogNote` now also checks a normalized form (trim +
  collapse whitespace + lowercase, via new `normalizeNoteContent`/
  `findNoteByNormalizedContent`) against existing notes of the same `kind`
  before inserting, so a paraphrased restatement ("user   likes   GO")
  resolves back to the same row as the original ("User likes Go") instead
  of creating a duplicate. Storage keeps the original first-logged casing
  (an initial version normalized on write too, which broke three existing
  tests asserting exact casing — reverted to compare-only). Does **not**
  strip punctuation (`"go"` vs `"go."` still differ) — a stated, deliberate
  scope boundary, not an oversight. The dedup check itself is an O(n) scan
  per `LogNote` call, non-atomic, so back-to-back concurrent differently-cased
  paraphrases could still race past each other into two rows — acceptable
  given `LogNote` isn't called concurrently in ora's current usage.
- **Vector index**: now built (see "Hybrid retrieval harness" below) — the
  original RAM budget in `PLAN.md` assumed 768-dim vectors, and the current
  default (768/1536/3072-selectable via MRL) gets picked based on the
  quality this needs, not a RAM ceiling — see correction below. At
  3072d/10k-node cap: **measured** (not computed) at a **254MB RSS delta**
  (`internal/vector/memory_footprint_test.go`, `vectorbench` build tag,
  2026-07-25) — see `HANDOFF.md` §6 for the full methodology and why this
  runs meaningfully higher than the earlier back-of-envelope ~123-150MB
  estimate. The number is real, it's just not the deciding factor (§3's
  quality-first directive still stands).

**Correction, same day — now FIXED in code**: I initially wrote off
`internal/config/gemini.go`'s `EmbedModel = "gemini-embedding-2"` as "not a
real model." Wrong — it's a real, GA (2026-04-22) omnimodal embedding model,
just on the wrong platform namespace for what ora actually calls. ora's genai
client is configured with `Backend: genai.BackendGeminiAPI` (confirmed in
`connect.go`/`compiler.go`) — the public Gemini Developer API, not Vertex
AI/Enterprise Agent Platform. `gemini-embedding-2` (bare) is the
**Vertex/Enterprise** identifier; the Developer API string for the same model
family is **`gemini-embedding-2-preview`**. Changed `EmbedModel` to that
value. Also applied the two other model corrections in the same file:
`TextModel` → `gemini-3.5-flash-lite` (supersedes 3.1, verified current,
same cost tier, no downside), `TTSModel` → `gemini-3.1-flash-tts-preview`
(the prior `gemini-2.5-flash-preview-tts` was NOT actually broken — verified
real and current — this is a quality upgrade: ~3x language coverage, inline
audio-tag control, chosen per the "never compromise quality for a soft RAM
preference" stance below). `go build`/`go vet`/`go test ./internal/...` all
green after the change; no test asserted the old string values.

**User correction, same day**: RAM/binary size is a soft "keep it lean where
free" preference for ora, not a hard constraint — never trade it against
quality. Output dimensionality should be picked for retrieval quality first
(Google's own guidance leans toward 1536 or full 3072 over 768 for
best-quality retrieval; MRL means truncating *does* cost some quality, just
less than a naive cut), with truncation considered only as a later, measured
optimization if RAM/disk actually becomes a real problem — not assumed
upfront.

Not urgent at 1 year of usage. Becomes real work by year 2-3: episode/notes
row-count growth needs an actual archival or cap policy, and the vector
index's pruning cap (designed, not yet implemented) is load-bearing once it
exists.

---

## Hybrid retrieval harness — IMPLEMENTED, 2026-07-25

The combined FTS5+vector harness designed above (chromem-go, RRF fusion,
domain boost-not-wall) is no longer prospective — it's built, TDD-verified
(`go build`/`go vet` clean, full `go test ./internal/... -count=1` green,
`-race` clean on `internal/db`/`internal/agent`/`internal/vector`/
`internal/embed`/`internal/memory`), via a strict two-phase build (one pass
wrote the full locked test suite + signature stubs, a second implemented
against it without editing a single test assertion). New: `internal/embed`
(swappable `Embedder`, `GeminiEmbedder` — asymmetric `RETRIEVAL_DOCUMENT`/
`RETRIEVAL_QUERY` task types, confirmed live against the real API at
3072-dim default), `internal/vector` (swappable `Index`, `ChromemIndex` —
chromem-go v0.7.0, JSON sidecar for restart-surviving LRU eviction at the
10k-node cap), `internal/memory.Classify` (work/personal lookup), and
`internal/db/hybrid.go` (`reciprocalRankFusion`, `HybridSearch`, wired into
the `query_memory` tool's new optional `domain` arg). `LogEpisode` tags
`domain` and async-embeds; `LogSemanticNode` inherits domain by majority
vote over its episodes; notes async-embed but aren't domain-tagged (scope
cut). See `HANDOFF.md` for the full narrative, including two real bugs
caught and fixed in post-implementation review: (1) the domain-boost path
only ever saw vector-sourced candidates' domain tags, never lexical/FTS5
ones, silently defeating the boost for the common case; (2) `Classify`'s
"code"/"word"/"excel" work-signal substrings collided with ordinary English
in free-text titles (Wordle, "excellent", "password", "decode" all
misclassified as work) — fixed by restricting those specific short/generic
terms to app-name-only matching.

**Update, 2026-07-25**: the two open gaps above are now closed. The
lexical-side hard domain filter has locked test coverage
(`TestHybridSearch_DomainFilter_ExcludesNonMatchingLexicalResult` in
`internal/db/hybrid_test.go`, red-verified: fails with a reproduced
regression when the filter is disabled, passes against the real code).
The HITL-blocked async tool-call goroutine leak (P1) is fixed: `executeTool`
now takes `ctx` and selects on `ctx.Done()` alongside the approval channel,
so a goroutine whose session dies before approval exits instead of leaking
— see `TestRunToolCall_SessionEndsBeforeApproval_GoroutineExitsInsteadOfLeaking`
in `internal/agent/connect_test.go` (also red-verified against a
goroutine-count regression). Notes/threads still carry no domain column at
all — that one remains open, an explicit scope cut, not a bug.

Also closed this pass (ponytail-audit findings, verified still applicable
after this arc's changes — some flagged items like `Retriever` had since
become genuinely load-bearing and were kept): deleted the dead
`Summarizer.Summarize` method (interface + `GeminiSummarizer` impl + 7 mock
stubs across `compiler_test.go` — `Compiler.processFlush` only ever called
`AttributeThreads`/`ReconcileNotes`), the dead `StateDeriver` interface
(`internal/memory/state.go` — `DeriveState` is called directly on the
concrete `*GeminiSummarizer`, never through the interface; `state_test.go`
now exercises the real method directly instead of a fake), and the dead
`Retriever` interface (`internal/db/db.go` — only a compile-time assertion,
never taken as a parameter). Also fixed: a duplicate `GetWorkingState` call
inside `GetImplicitContext`, a byte-for-byte duplicated `countWords` helper
in both `internal/memory` and `internal/db` (unified as exported
`memory.CountWords`), and a duplicated command-item slice literal in
`internal/ui/list.go` (`newCommandList` / `FilterCommands`).
