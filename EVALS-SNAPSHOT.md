# evals/ snapshot — HISTORICAL (2026-08-27)

> **This describes a suite that no longer exists.** `evals/` was deleted on 2026-08-27 and rebuilt from scratch; the current suite is eight tracks documented in [docs/evals.md](docs/evals.md). Keep this file for the numbers below, which are the last measurements of the old harness and are not comparable to anything the current one reports.

Deleted `evals/` (1,680 lines, 4 packages, 3 commits from Aug 18-19). This file records what it measured so the numbers aren't lost.

## What it was

- `harness/` — opened a `VACUUM INTO` snapshot of the real `ora-db/db`, or a throwaway sqlite. Never touched the live file.
- `dataset/` — 17 seeded facts with probe queries; 23 companion questions (some curated in `questions.json`, some generated at runtime from working state, live threads, latest episode).
- `retrieval/` — drove the real `query_memory` and `recall` tools, scored recall@5 and MRR.
- `companion/` — for each question, asked which surface held the answer: frozen handshake, `RetrieveRelevant` inject, `query_memory`, or `get_recent`.

## Last numbers

```
retrieval scorecard:  recall@5  0.765 (13/17)    MRR 0.721
companion probe:      handshake 8 / retrieve 6 / tools 11 / miss 6  (of 23)
```

`minRecallAt5 = 0.55` was the only real regression gate in the suite.

Named misses at deletion time:
- "how should git commits be written" -> `no memory matches`
- "which a11y flag is safe to enable" -> `no memory matches`
- "how should new ORA features be implemented" -> wrong hit, right note not in top-5
- "how many layers should ORA's memory model have" -> right note returned, ranked below a generic thread

## Why it was green while retrieval was broken

The important finding, and the reason it wasn't worth keeping as-is.

1. **No embedder, no vector index.** `grep SetEmbedder evals/` returned nothing. The harness opened `db.New(path)` and stopped. Every number above is lexical-only (BM25) retrieval, not the hybrid system.
2. **No timeout.** `ProbeCache` called `store.RetrieveRelevant(ctx, ...)` on a bare `context.Background()`. Production calls it under `textSendLoopRetrieveTimeout = 500ms` (`internal/agent/connect.go:565`), which was failing on 100% of typed turns.

Those two omissions erased exactly the two things that were broken. The eval printed `ret=true` ("RetrieveRelevant would inject it") for tingira, pune, nietzsche, scrapped-pet and suits — none of which were actually injected in production.

3. **Only one test could fail on quality.** `TestCacheProbe_ProductionSnapshot` failed only if `hs == 0`. The 6 misses out of 23 were `t.Logf` and failed nothing.
4. **The live voice-vs-text comparison never ran** — gated behind `ORA_EVALS=1` + `GEMINI_API_KEY`, silently skipped by `go test ./...`.
5. **Auto-generated graders.** `distinctive()` took the 3 longest non-stopword tokens from a thread's state text and called those the expected answer — a substring match that passes on coincidence.

## If it comes back

Three changes make it honest: wire a real embedder and vector index into the snapshot harness; run `RetrieveRelevant` under the same 500ms budget `connect.go` uses; give the companion probe a miss ceiling that actually fails the test.

## Open memory defects it should have caught

1. `internal/agent/tools.go:501` — `recall` omits `NewestFirst`, so `internal/db/db.go` sorts `created_at ASC` and returns the OLDEST 50 of any window. Every real day exceeds 50 episodes, so "what did I do today" was structurally unanswerable.
2. `tools.go:511` — timestamps rendered in the stored location (UTC), not local. Every time shown was 5h30m early and could cross the date line.
3. `connect.go:565` — 500ms is below the floor for a remote Gemini embed round trip.
4. `internal/db/db.go:999` — `UpdateNote` discards `sql.Result`, never checks `RowsAffected`; a hallucinated note id returned "updated".
5. `tools.go:464` — `recall` with `subject` silently discards `since`/`until`.
