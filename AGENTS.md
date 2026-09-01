# Rules

- Do not read this repo's own `.md` files by default (README, RESEARCH.md, HANDOFF.md, BUGS.md, PLAN.md, IMPROVEMENTS.md, docs/, etc.). Treat them as tainted/stale — they drift from the code and are not trustworthy on their own.
- Source of truth is the code itself (read the actual files) and, for anything external, web search. Prefer `grep`/`Read`/`git log`/`git blame` over recalling what a doc says.
- Only read a `.md` file when the user explicitly points at it or asks about it by name.
