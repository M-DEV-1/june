# Evals

`go run ./evals -tracks <n[,n...]>` — writes a scorecard to `evals/runs/<date-time>-<sha>.md`.

Everything reads a `VACUUM INTO` snapshot of the live sqlite store, opened read-only. The vector half goes over the daemon's `/vector/search` endpoint, because two processes must never open the chromem directory at once. Nothing in any track writes to live data.

Requires `GEMINI_API_KEY` in `.env` for the judge. **That key is also the memory compiler's** (`cmd/daemon.go`) — see "The quota collision" below before running a campaign.

## The tracks

| # | Name | Measures | Keep? |
|---|---|---|---|
| 1 | memory replay | Could the retrieved rows answer the question? No model reasoning about the answer, so it is the most trustworthy number available. | yes — the core number |
| 2 | conversation | Taste criteria over real turns from `ora.log`. Only covers turns logged since 2026-08-28. | yes, slower cadence |
| 3 | minutes | `minutes.md` against the minutes prompt's rules, with `transcript.md` alongside so a claim can be checked. | yes — but re-baseline (below) |
| 5 | counterfactual replay | Claude as teacher against the live model, using the live session's frozen tool results. | retire — superseded by 7 |
| 6 | distillation study | Not a measurement. Reads track 5's files and writes lessons to `~/.local/share/ora/study`. | move out of the runner |
| 7 | trajectory | Two arms run real agentic loops against a snapshot, writes stubbed, a third model roleplaying the user. | yes |
| 8 | context vs capacity | Separates a retrieval failure from a model failure. | yes |

Only tracks 1–3 write a headline row. A track-7-only run therefore produces a scorecard whose headline table is empty — that is expected, not a failure.

Track 3 wants re-baselining: its 70% → 100% jump was measured when a meeting transcript was one undifferentiated paragraph, so the "model ceiling" reading may be a context problem wearing a model problem's clothes.

## Track 8 — context vs capacity

```
go run ./evals -tracks 8 -questions evals/probes.jsonl
```

Scores two axes against the **same frozen rows**:

- a judge says whether the rows were sufficient (the context axis — no arm influences it)
- three model families answer from those exact rows and are judged on what they said (the model axis)

Crossing the two separates a question the material could never have answered from one the model failed to answer.

|  | model answered well | model answered badly |
|---|---|---|
| **rows sufficient** | working | **model failure** — retrieval work is wasted here |
| **rows insufficient** | answered anyway — check for a guess from training data | **context failure** — every arm fails identically |

Arms are `claude-sonnet`, `grok` and `agy`, all on subscriptions the machine already pays for — no API spend. Three *families* rather than one vendor's small-versus-large ladder, because a gap inside one lab is confounded by that lab being weak at the question shape.

Rows and every answer are frozen to `evals/runs/<date-time>-<sha>-track8.json`, so a run can be re-judged or given a fourth arm without spending another round of calls.

### The question sets

- `evals/questions.jsonl` — 24 questions harvested from real usage
- `evals/probes.jsonl` — 25 probes built by *reading the database*, which is the only way to write a question that should be refused: it needs a real absence (vision starts 18 Aug, the diary has 3 days, minutes start 28 Aug)

Every question carries `asked_at`, and the judge is told it. Without that, a question saying "today" is scored against whenever the eval happens to run — five of the 24 were measuring a different window on every run.

Probes also carry `expect` (`answerable` / `hard` / `clarify` / `refuse` / `false-premise`), which is **never shown to any judge or arm**. Telling a judge a question is meant to be refused makes it rubber-stamp any refusal.

## Traps, found by running it

- **A judge outage scored as a model failure.** `{verdict:"fail", why:"judge call failed"}` put every API error into the cell that reads "the material was there and the model failed to use it". Now excluded from rates and counted in the scorecard. *The arm path still has this bug* — a missing CLI binary or a timeout is scored as a bad answer.
- **Asking a judge for a `declined` boolean does not work.** All three arms replied literally `INSUFFICIENT` to a question about a person who does not exist, and the judge reported `declined=false` for all three. Refusal is now read from the arm's own text.
- **Run files were named `<date>-<sha>`**, so a second run of the same commit in one evening silently overwrote the baseline it was being compared against. Now to the minute.
- **Do not generate questions from the corpus.** Every question becomes answerable by construction, and generated questions are clean where real ones are elliptical and code-switched. Harvest from `ora.log`, or have a model *classify* existing material — never author.
- **The eval can measure a path the product does not use.** `directSearch` calls `HybridSearch(question, "", 10)` — the raw question with its time words and no window — while `query_memory`'s own description says the opposite: pass `since`/`until`, never put a time word in the query. Two probes were penalising retrieval for a mistake the product is told not to make.

## The quota collision

`cmd/daemon.go` and `evals/main.go` both read `GEMINI_API_KEY`. The memory compiler and the eval judge share one free-tier quota, so **measuring Ora degrades Ora**: three runs in an evening exhausted it, `AttributeThreads` began failing 429, threads stopped being upserted, and summaries fell back to raw activity logs — silently, because the compiler falls back and keeps running.

Until the compiler moves onto `brain.FromConfig` (already `claude-cli` on this machine), budget roughly one full run per day and check `ora.log` for `429` afterwards.

## Reading a result honestly

A single accuracy number renders "I don't know" and "I'm wrong" identically. Measured across three families on 18 answerable probes, neither claude nor grok gave a single wrong answer — every failure was a refusal, and grok's lower score was calibration, not capability. Score refusal separately or the conclusion inverts.
