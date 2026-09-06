/** What sits under one of Ora's replies in the thread: the grey line saying when it was said and what it read, folded away behind a click. */

import { ChevronDown, ChevronRight } from "lucide-react";

import type { Evidence, Turn } from "./api";
import { hhmm } from "./format";
import { ui, useAppDispatch, useAppSelector } from "./store";

/** One of the quotes behind an answer, drawn as the thing that was read rather than as a field: the source and when it was on one grey line, the words themselves on a sunken block under it. Input: the rows the daemon sent. Output: the stack of them. */
export function Quotes({
  evidence,
  compact,
}: {
  evidence: Evidence[];
  compact?: boolean;
}) {
  return (
    <div
      className={compact ? "flex flex-col gap-3" : "mt-3 flex flex-col gap-2"}
    >
      {evidence.map((e, i) => (
        <div
          key={`${e.title}-${i}`}
          className={
            compact ? "border-l pl-3" : "rounded-md bg-sunken px-3 py-2"
          }
        >
          <div className="flex justify-between gap-3 text-meta text-muted-foreground">
            <span className="truncate" title={e.title}>
              {e.title}
            </span>
            {/* "meeting · 3 Sep" is a label for where the quote came from, not a machine-readable time, so it is not a <time>. */}
            <span className="shrink-0">{e.meta}</span>
          </div>
          <p
            className={
              compact ? "mt-1 text-meta text-foreground" : "mt-1 text-read"
            }
          >
            {e.body}
          </p>
        </div>
      ))}
    </div>
  );
}

/** The row that folds a reply's sources away and back. Input: how many there are, whether they are showing, what the row is called, the id of the block it opens, and what to do when it is clicked. Output: the row — a chevron, the word, and the count — which is the same control on an answer's quotes and on a failed ask's provider message. */
function Fold({
  label,
  count,
  open,
  controls,
  onToggle,
}: {
  label: string;
  count?: number;
  open: boolean;
  /** The id of the block this row opens, so aria-expanded says what it is expanding. */
  controls: string;
  onToggle: () => void;
}) {
  return (
    <button
      type="button"
      aria-expanded={open}
      aria-controls={controls}
      onClick={onToggle}
      className="-ml-1.5 mt-1 flex items-center gap-1 rounded-sm px-1.5 py-1 text-meta text-muted-foreground outline-none transition-colors hover:bg-hover hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring"
    >
      {open ? (
        <ChevronDown className="size-3.5" />
      ) : (
        <ChevronRight className="size-3.5" />
      )}
      {label}
      {count === undefined ? null : (
        <span className="rounded-xs bg-muted px-1 tabular-nums">{count}</span>
      )}
    </button>
  );
}

/** What sits under one of Ora's replies: the grey line saying when it was said and which tools it called, and the fold holding what it read. Input: the turn. Output: the line, the fold, and the quotes themselves when the fold is open. A failed ask gets the same shape, except that what the fold holds is the provider's whole message rather than what was read. */
export function ReplyMeta({ turn, folded = true }: { turn: Turn; folded?: boolean }) {
  const dispatch = useAppDispatch();
  const open = useAppSelector((s) => s.ui.openRails.includes(turn.id));
  const time = hhmm(turn.when);

  if (turn.kind === "error") {
    const whole =
      (turn.text ?? "").trim() !== (turn.reason ?? "").trim() &&
      (turn.text ?? "").trim().length > 0;
    return (
      <>
        <div className="mt-1.5 text-meta text-muted-foreground">
          {time} · could not answer
        </div>
        {whole && folded ? (
          <Fold
            label="The whole message"
            open={open}
            controls={`fold-${turn.id}`}
            onToggle={() => dispatch(ui.railToggled(turn.id))}
          />
        ) : null}
        {open && whole && folded ? (
          <pre id={`fold-${turn.id}`} className="mt-2 max-h-[220px] overflow-auto rounded-md bg-sunken p-3 font-mono text-meta whitespace-pre-wrap">
            {turn.text}
          </pre>
        ) : null}
      </>
    );
  }

  const evidence = turn.evidence ?? [];
  const tools = (turn.tools ?? []).filter(Boolean);
  return (
    <>
      <div className="mt-1.5 text-meta text-muted-foreground">
        {time}
        {tools.length && folded ? ` · ${tools.join(", ")}` : ""}
        {evidence.length ? "" : " · read nothing — treat it that way"}
      </div>
      {evidence.length && folded ? (
        <Fold
          label="Sources"
          count={evidence.length}
          open={open}
          controls={`fold-${turn.id}`}
          onToggle={() => dispatch(ui.railToggled(turn.id))}
        />
      ) : null}
      {open && evidence.length && folded ? (
        <div id={`fold-${turn.id}`}>
          <Quotes evidence={evidence} />
        </div>
      ) : null}
    </>
  );
}
