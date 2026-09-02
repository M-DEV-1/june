# Testing strategy

Companion to [evals.md](evals.md). That file covers the LLM-judged tracks; this one covers everything below them and, more importantly, the layer that is missing between the two.

## Where the suite stands

```
763 unit test functions          33 table subtests          8 eval tracks
```

Coverage by package, lowest first:

| package | coverage | note |
|---|---|---|
| `cmd` | 27% | daemon wiring — where the event loop lives |
| `internal/audio` | 30% | hard to test without a sound server |
| `internal/tracker` | 41% | the capture layer everything else eats from |
| `internal/config` | 67% | |
| `internal/memory` | 67% | the compiler |
| `internal/agent` | 72% | tools and the live session |
| `internal/db` | 76% | |
| `internal/recorder` | 80% | |
| `internal/dream`, `brain`, `tally`, `study`, `embed` | 84–90% | |

The two lowest — `cmd` at 27% and `tracker` at 41% — are where the two most damaging bugs of 2026-08-31 lived. That is suggestive but it is not the real lesson.

## The real lesson: unit tests are blind to this system's failure mode

Five bugs were found on 2026-08-31/09-01. Ask of each: would a unit test have caught it?

| bug | caught by a unit test? | why not |
|---|---|---|
| every memory row truncated to 200 runes | **no** | `FormatHit` did exactly what it said. The constant was wrong, not the code. |
| the clock dropped at render (`Mon Jan 2`) | **no** | The format string was "correct". Nothing asserts a row can answer a time-of-day question. |
| 261 threads with no edge to 4,901 episodes | **no** | The feature did not exist. You cannot unit-test an absence. |
| the meeting window never captured | **no** | `extractText()` correctly returned the *focused* window's text. Focused was the wrong window. |
| `FormatNoteHit` diverging from `FormatHit` | **yes** | A consistency assertion between two formatters catches this immediately. |

One in five. 763 unit tests did not catch the other four, and more unit tests would not have.

The reason is structural. This system's failures are not "this function computes the wrong value" — they are **"a correct function was given the wrong job"** and **"data was dropped crossing a boundary"**. A unit test asserts a function does what it says; both of those bugs pass that assertion trivially.

## The missing layer: contract tests

Between the unit tests and the LLM-judged evals there should be a layer of cheap, deterministic, no-model tests that assert *invariants across component boundaries*. This is the layer that maps to the actual bug pattern, and it does not exist yet.

Four invariants worth pinning, each traced to a real bug:

**1. Every read path renders the same source the same way.**
`query_memory` sends notes to `FormatNoteHit` and everything else to `FormatHit`; the eval sends everything to `FormatHit`. That one-line divergence meant the eval reported a fix the product never received.

```go
// for each source in {episode, note, summary, thread, diary}:
//   assert the excerpt budget FormatHit gives it == the budget FormatNoteHit gives it
```

**2. A rendered row carries everything a question could need from it.**
Not "the format string is right" but "a row can answer the questions we ask of rows". A row must carry a resolvable timestamp including the hour; a note row must carry its `[note#N]` so a correction tool can address it.

**3. What a component decides is persisted somewhere.**
The compiler computes the episode-to-thread attribution on every flush and discarded it for months. The daemon's `if _, err := store.WriteEpisode(...)` dropped the id one line before the compiler needed it. An invariant test — *after a flush, every attributed thread has at least one linked episode* — catches the whole class.

**4. The eval harness and the product call the same code.**
`directSearch` passes the raw question with its time words; `query_memory`'s description forbids exactly that. Assert that the harness's retrieval path and the agent's produce identical output for identical input, or the harness is measuring a neighbour of the real thing.

These are ordinary Go tests. No API key, no judge, no flakiness, runs in CI.

## What each layer is for

| layer | count | what it proves | when it runs |
|---|---|---|---|
| unit | 763 | a function does what its name says | every build |
| **contract** | **0 today** | **components agree at their boundaries** | **every build** |
| eval tracks 1, 8 | 2 | retrieval finds it, and the model can use it | manual, quota-bound |
| eval tracks 2, 3, 7 | 3 | the system behaves well over real material | slower cadence |

## Gaps beyond the contract layer

**`cmd` at 27%.** The daemon's event loop is nine lines that decide what gets persisted and what reaches the compiler, and both of today's data-loss bugs passed through it. It is testable — extract the loop body into a function taking a store and a compiler interface.

**`internal/tracker` at 41%.** `extractMeetingWindow` and `watchMeetingWindow` are new and covered only by the compile. The D-Bus walk cannot be unit-tested without a bus, but the *selection* logic can: given a fake tree, does it pick the meeting window? Does `lastText` advance only on a successful send?

**Nothing tests the recorder end-to-end.** Whisper is behind a seam and stubbed, which is right, but there is no test that a WAV pair plus a diarization file produces a sensible transcript. The fixtures exist — `evals/runs/*-track8.json` holds real frozen output.

**Non-determinism has no policy.** The eval tracks call models at temperature 0 with a judge that can disagree with itself, and there is no rule for what counts as a regression. Gemini CLI's answer is a "50% rule" — a behavioural test passes if the correct behaviour occurs in at least 2 of 4 attempts, with tests split into always-passes (blocks CI) and usually-passes (nightly). Worth copying rather than inventing.

## Targets

Not a coverage percentage — that would reward testing getters. Instead:

- **Every cross-component invariant above has a test.** Four to start.
- **Every bug found in production gets a test at the layer that would have caught it**, which for this system is usually the contract layer, not the unit layer.
- `cmd` and `internal/tracker` above 60%, reached by extracting the testable logic rather than by mocking D-Bus.
- Eval tracks 1 and 8 run on the same commit before and after any retrieval change, with the frozen rows kept so the comparison costs no model calls.
