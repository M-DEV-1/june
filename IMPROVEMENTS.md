# ORA Memory Improvements

Research notes + improvement plan after inspecting the live `ora-db/`, logs, and retrieval paths (2026-07).  
**No commits implied.** This is the working diagnosis and the design to fix it.

Tagline candidate for the eventual write-up: *Walking Up the Wrong Trees.*

---

## 1. Is the cause of all our problems… lack of semantic search?

**No.**

Semantic search (embeddings + HNSW / hybrid) is a *later amplifier*. It is not the root of the failures we measured. Most failures are:

| Failure mode | Needs vectors? | Actual cause |
|---|---|---|
| `query_memory("GPU infra")` → no matches, while 51 GPU docs exist | **No** | Entire query string is FTS5-quoted as one **phrase** |
| `recall(subject="July 5th")` → empty / wrong | **No** | Temporal question forced through **keyword subject path**, not day window |
| `what did i do yesterday?` | **No** | Tree is never walked; turn FTS gets natural-language phrase |
| Working-state as focusSignal → 0 FTS hits | **No** | 900-char essay phrase-MATCHed |
| Summaries in FTS as raw JSON | **No** | `json.Marshal(TaskSummary)` indexed verbatim |
| YouTube title invisible to episode FTS | **No** | Only `screen_text` is in `episodes_fts`; **title/app not indexed** |
| Pre–July-5 “no series” | **Partly storage** | Episodes substrate didn’t exist yet; summaries existed but weren’t retrieved |
| “I don’t remember last week” feeling | **No** | Data often *is* in the day tree; retrieval never walks it |

**Where vectors *would* help (after plumbing is fixed):**

- Paraphrase: “what series am I watching?” ↔ thread/summary text that never shares tokens with the question.
- Fuzzy topic: “that CUDA rabbit hole” ↔ episodes titled / described differently.

**Rule of thumb:**  
If token FTS (OR of significant tokens) + tree walk + clean indexed text still fails on true paraphrases, *then* add vectors behind the existing `Retriever` interface. Shipping vectors on top of phrase-FTS + JSON blobs + unindexed titles is walking up the wrong tree with a fancier ladder.

---

## 2. What we already have (so we stop redesigning capture)

### Capture harness — solid

- Dwell-confirmed activity, blocklist, media-aware (MPRIS) vision escalation.
- Tiered capture: accessibility first; screenshot → `DescribeScreen` when thin / media.
- Episodes: append-only, importance heuristic, separate FTS, aging policy.

### Dual substrate — intentional, incomplete wiring

| Layer | What it is | In tree? | FTS? |
|---|---|---|---|
| **nodes** | USER → DAY → SESSION → TASK → summary/(digest) | Yes | summaries/digests in `memory_fts` |
| **episodes** | Flat time series of captures | **No** | `episodes_fts` on `screen_text` only |
| **threads** | Throughlines (show, PR, project) | Side table | subject + state |
| **notes** | Stable identity facts | Side table | content |
| **working_state** | Single-row “right now” | Side table | not FTS |

### Live DB snapshot (inspected)

- Summaries back to mid-June (hundreds of task labels). **Previous days are not empty.**
- Episodes only from ~2026-07-05 onward (rich dual layer is recent).
- **0 digests** — day rollup compaction has not left durable day-level prose in the tree.
- Threads are sharp (Suits, Langfuse, LeCun, etc.) when the model actually sees them.

So the hurt feeling (“I thought we stored previous days”) is half right:

- **Stored:** yes — as tasks + JSON summaries under day/session nodes.
- **Recoverable as a day story:** no — nothing walks `day(content=YYYY-MM-DD)` → tasks → summaries in order, and episodes don’t exist for older days.
- **Recoverable as hour-level replay:** only for episode-era days, and only if `EpisodesInWindow` is used with consistent timestamps.

---

## 3. Walking up the wrong trees

### The tree we have

```text
user
 └── day (2026-07-05)
      └── session ("Active Session")   ← always this name; weak signal
           └── task ("Suits")
                └── summary (JSON blob)
                └── summary ...
           └── task ("Langfuse ...")
                └── ...
```

### The walk we do today

`GetImplicitContext` is **not** “open yesterday’s day node and walk children.”

It approximately:

1. Builds a long **focusSignal** from working_state + last tasks.
2. Phrase-FTS via `RetrieveRelevant` (often empty).
3. Appends **live threads** (last 2 days) + **[now]** working_state.
4. Early-returns. The recursive CTE that walks ancestors of recent summaries is only a **cold-start fallback**.

So when you ask “what did I do on July 5th?”, the system is doing sparse keyword search over a global bag of JSON and screen chrome — **not** walking the July 5 day subtree.

### Episodes are off-tree by design

`episodes` deliberately skip the unique `(parent_id, type, content)` index so revisits don’t collapse. That was correct for append-only capture.

The mistake is leaving them **orphaned from temporal navigation**:

- No `day_id` / `session_id` link.
- Timeline tools query a flat `created_at` range (and UTC formatting can skew IST wall-clock storage).
- Day tree walk never includes episodes.

### The simple mental model we should implement

**Two retrieval primitives only:**

1. **Walk (temporal / structural)**  
   Given a time range or day key → ordered children (tasks → summaries; plus episodes in range).  
   Answers: yesterday, this morning, July 5th, “walk me through my day.”

2. **Index (topical / sparse)**  
   Given a focus (user utterance or current activity keys) → top-k notes/threads/summaries/episodes.  
   Answers: “GPU infra?”, “that Suits episode”, “Langfuse PR.”

Everything else (tools, handshake context, turn injection) is a thin caller of these two.  
Today we fake both with one broken FTS phrase MATCH and hope the model invents the right tool args.

---

## 4. “One for all” observation model (not per-platform)

You are right: YouTube / Prime / Netflix / Instagram / Twitter / news / Pinterest cannot each get bespoke parsers. Media is mixed: video-only, image+text, text-only, canvas games.

### What capture actually produces

Tiered path is already platform-agnostic:

1. **Title + app** — always available from the window manager.
2. **Accessibility tree text** — free; often chrome + U+FFFC (object replacement for images/widgets) + sometimes real copy.
3. **Vision line** (`DescribeScreen`, ≤~100 words) — when a11y is thin or media is active; this is the *semantic* observation for pixels.

For video/image surfaces, raw `screen_text` is frequently **BS with a few jewels** (video title fragment, timestamp, “S3 E13 Moot…”). Vision prose is the signal. For code/docs/terminals, a11y/text is the signal and vision is optional.

### Universal write path (less is more)

Do **not** branch on host. On every episode write, normalize into one shape:

```text
Observation {
  app, title,          // always first-class
  signal,              // cleaned primary text for memory + FTS
  signal_kind,         // "vision" | "a11y" | "title_only"
  raw_len,             // optional metrics; raw blob not required in FTS
}
```

**`signal` construction (one rule for all platforms):**

1. Strip U+FFFC and other replacement/control runs; collapse whitespace.
2. If vision description exists (or screen_text looks like vision prose), **prefer it as `signal`**.
3. Else take a11y text **capped** (e.g. first N words / after chrome strip), not multi-KB trees.
4. Always concatenate **`app + title + signal`** into the FTS document (and into any future embedding text).
5. Optionally keep full raw off the hot path (or age it aggressively). Indexed memory should stay short.

Why this is one-for-all:

- Instagram carousel, YouTube, newspaper, IDE — same fields.
- Title carries “Prime Video: Suits Season 3” even when the pixel tree is garbage.
- Vision carries “watching Suits, Rachel Zane scene…” when a11y is chrome.
- Code editor carries real a11y/text in `signal` without vision cost.

**U+FFFC:** yes — primarily image/embedded-object placeholders in accessibility trees. Stripping them is not a YouTube hack; it’s hygiene for every GUI. Raw trees stay useful only *after* parse/clean; otherwise they dilute BM25 and bloat storage.

**Less is more:** better one 80-word observation than a 8KB a11y dump that FTS ranks by “Skip to main content.”

---

## 5. IndexShare & Dynamic Sparse Attention — useful metaphors, not copy-paste

What you were reading about (GLM-5.2 / DSA lineage):

- **Dynamic Sparse Attention:** each query does not attend to all past tokens; an indexer picks a small top-k set.
- **IndexShare:** the expensive “which past tokens matter?” decision is computed **once per block of layers** and **reused**, instead of re-indexing every layer.

ORA is not a transformer. The transferable ideas:

| Transformer idea | ORA analogue |
|---|---|
| Full attention over history | Dumping all notes / all live threads / long working state into the Live prompt |
| Sparse top-k attention | `RetrieveRelevant` / tools return **small** ranked sets |
| Indexer | One **FocusIndex**: token FTS (+ later embeddings) over a **clean** corpus |
| IndexShare | Compute focus **once per turn** (or once per connect), reuse for handshake inject, turn inject, and tool defaults — don’t invent three different broken queries |
| KV aging / compression | Digests + AgeEpisodes + note consolidation (you already sketched this) |

**Design target:** a single sparse retrieval spine:

```text
focus = extract_keys(user_text | current_activity | short task names)
        # NOT the full working_state essay

hits  = Index(focus, k)           # sparse topical
walk  = Walk(since, until)        # temporal (if question is temporal)

context = merge(hits, walk) with hard caps
```

That is “IndexShare for episodes + DSA for the prompt” without embedding a model architecture into the daemon.

Vectors later become just another indexer backend behind `Retriever`. The share/sparse contract stays.

---

## 6. Root-cause map (fix order, still no vectors required)

### P0 — Retrieval plumbing (unblocks almost all log failures)

1. **FTS query builder**  
   - Tokenize; drop stopwords; OR significant tokens.  
   - Phrase MATCH only for explicit multi-word quotes.  
   - Never phrase-wrap full working_state or full user sentences.

2. **Index human text for summaries**  
   - Store/display/index `task_name — summary`, not `json.Marshal(TaskSummary)`.  
   - (JSON can remain internal if needed; FTS + tool output must be prose.)

3. **Episode FTS document = `app + title + signal`**  
   - Not screen_text alone.  
   - `signal` = cleaned vision-or-a11y (section 4).

4. **Temporal primitive: Walk**  
   - `DayArc(date)` / `EpisodesInWindow` with **one clock** (UTC everywhere or local everywhere; stop `.UTC()` formatting against local-looking SQLite strings).  
   - `recall`: if subject parses as a date / relative day, route to Walk, not subject FTS.  
   - Tool + prompt: calendar questions → `since`/`until` only.

5. **Tree walk for pre-episode history**  
   - `SummariesForDay(day)` ordered by time.  
   - “What did I do on June 17?” should return that day’s tasks/summaries even with zero episodes.  
   - This is how you honor the data you already paid to store.

### P1 — Sparse context injection

6. **Focus from activity keys**, not working-state essay. Keep `[now]` as prose only.  
7. **Budget:** reserve slots for episodes/threads; don’t fill maxItems with JSON summaries first.  
8. **Turn recall:** slightly higher k for temporal cues; still hard-capped (DSA, not dump).  
9. **One FocusIndex per turn** shared by inject + default tool hints (IndexShare analogue).

### P2 — Substrate quality (less is more)

10. **Normalize on write** (section 4): strip U+FFFC, cap a11y, prefer vision for pixel-heavy screens, always keep title.  
11. **Digests actually fire:** compaction exists; DB had **0 digests**. Make day rollups real so old days shrink to walkable prose.  
12. **Optional:** soft-link episodes to `day_id` for simpler joins (still append-only; no unique collapse).

### P3 — Only after P0–P2 feel good in real chats

13. Embeddings + hybrid search behind `Retriever`.  
14. Embed the same short `signal` documents, not raw a11y dumps.

---

## 7. “Previous days” — honest inventory

| Era | What you have | What recall can do once Walk works |
|---|---|---|
| Pre–episode (≈ June) | Tasks + summaries under day nodes; some threads/notes | Day story from labels + prose; no hour-level screen replay |
| Episode era (≈ July 5+) | Above + rich episodes | Hour-level walk + topical sparse index |
| Digests | Schema + code; **empty in live DB** | Should become the long-term day leaf |

Nothing magical was deleted for June task/summary history — retrieval simply never opened those day nodes. Episodic richness is newer; that gap is real and should be named in product expectations (“I remember your July in high resolution; June is in day notes”).

---

## 8. Success criteria (manual, same questions as the logs)

After P0–P1, without vectors:

- [ ] `query_memory("GPU")` / `"GPU infra"` surfaces DGX/Gemma/vLLM era summaries.  
- [ ] `recall(since=2026-07-05, until=2026-07-05)` returns ~day’s episodes in order (timezone-correct).  
- [ ] “What did I do on June 17?” walks that day node and returns tasks/summaries.  
- [ ] “What series am I watching?” hits Suits thread + title/signal-indexed episodes.  
- [ ] Handshake context stays small, sparse, non-JSON.  
- [ ] No tool result that is a raw `{"same_task":...}` blob.

If those pass and paraphrase still fails, *then* vectors earn their keep.

---

## 9. Explicit non-goals (for now)

- Per-site scrapers (YouTube vs Instagram).  
- Full vector DB servers / CGO.  
- Dumping more into the Live system prompt.  
- Renaming the blog post before the tree walk works.

---

## 10. One-paragraph north star

ORA already captures a dual memory; the bug is that **retrieval attends to the wrong document with the wrong query and never walks the day tree**. Fix the indexer (clean short observations, real FTS queries), share one sparse focus per turn, and make temporal questions a tree/time walk. That is the IndexShare + DSA idea at companion scale. Semantic search is the upgrade once the walk and the index point at the same, honest text.

*Walking up the right trees — fewer leaves, better paths.*
