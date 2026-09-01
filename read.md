# ORA — Reading List

Sources behind ORA's memory + capture design. Grouped by theme, with a one-line
"why it matters for us." Entries marked **(verify)** are ones I could not firmly
confirm the exact URL for — search the title if the link rots.

## Ambient capture / screen-memory tools (the "all-seeing" peers)

- **Screenpipe** (open source, closest sibling) — https://github.com/mediar-ai/screenpipe
  Accessibility-tree-first capture with OCR fallback only when blind; local
  SQLite; **event-driven** capture (app switch / focus / typing pause), not a
  fixed timer. This is the design we should study most.
- **Rewind.ai** — https://www.rewind.ai/
  Screenshot + OCR + audio, fully local, vector store. The "capture everything as
  pixels" approach — useful as the *contrast* to our a11y-first stance.
- **Limitless** (Rewind's successor + pendant) — https://www.limitless.ai/
  Audio-first lifelog. Shows the recall UX bar.
- **Microsoft Recall** — https://learn.microsoft.com/en-us/windows/ai/apis/recall (verify)
  Screenshot-every-~5s + local NPU OCR + semantic index. Also a cautionary tale:
  security incidents from raw data leaking out of the "protected" store.
- **ActivityWatch** — https://activitywatch.net/ · repo https://github.com/ActivityWatch/activitywatch
  Window-title + AFK watchers, append-only event buckets. The minimal end of the
  spectrum (no AI, no forgetting).

## Agent memory architectures (the 3-layer model)

- **Cognitive Architectures for Language Agents (CoALA)** — https://arxiv.org/abs/2309.02427
  The canonical framing of working / episodic / semantic / procedural memory for
  agents. This is the theory behind our 3 layers.
- **Generative Agents (Park et al., 2023)** — https://arxiv.org/abs/2304.03442
  The memory stream + retrieval score = **recency-decay + importance + relevance**,
  and "reflection" (rolling up observations into higher-level insights). Directly
  shapes slice 3 (compaction) and slice 4 (recall ranking).
- **MemGPT / Letta** — paper https://arxiv.org/abs/2310.08560 · project https://github.com/letta-ai/letta
  Two-tier "main context (RAM) vs external store (disk)" with the agent paging
  data between them and summarizing on overflow. The working-state-as-cache idea.
- **MemTier** (2025 preprint) — search: https://scholar.google.com/scholar?q=MemTier+agent+memory+cognitive+weight (verify)
  Episodic/semantic/procedural tiers with a per-entry **cognitive weight** that
  rises on useful recalls and decays otherwise; background consolidation daemon.
  Source for the "smarter-than-age culling" idea in slice 3. *I could not firmly
  verify this exact title — treat as a lead, not gospel.*

## Memory in shipped consumer AI

- **OpenAI ChatGPT Memory (FAQ)** — https://help.openai.com/en/articles/8590148-memory-faq
  Key-value "saved memories" with upsert-by-key. The pattern for our notes/truth
  layer (reconcile, don't append).

## Storage & retrieval background (the analogies I used)

- **SQLite FTS5** — https://www.sqlite.org/fts5.html
  ORA's keyword recall engine (`memory_fts`). Why we defer vector search.
- **Okapi BM25** — https://en.wikipedia.org/wiki/Okapi_BM25
  The ranking function FTS5 approximates; why it beats embeddings at our scale.
- **Log-Structured Merge-tree (LSM)** — https://en.wikipedia.org/wiki/Log-structured_merge-tree
  The compaction analogy for the episodic layer: fresh writes on top, merged and
  coarsened downward over time.

## ORA's own design docs (in this repo)

- `RESEARCH.md` — running design notes + backlog user stories.
- `CLAUDE.md` — architecture, invariants, memory model.
- `internal/tracker/CONTEXT.md` — capture-layer notes.
