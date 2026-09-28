/** What sits under one of Ora's replies in the thread: the grey line saying when it was said and what it read, folded away behind a click. */

import { useState } from "react";
import { ChevronDown, ChevronRight } from "lucide-react";

import type { Evidence, Turn } from "./api";
import { hhmm, keyed } from "./format";
import { ui, useAppDispatch, useAppSelector } from "./store";

/** How many sources the rail shows before folding the rest behind "show all": a rail beside a wide thread has a capped, scrolling height, but a reply with dozens of sources should not hand a reader that much to scroll through before they can even see what it did. The full window's own fold under a reply (ReplyMeta, unaffected by this) already asks a click to see any of them, so it lists every one once opened. */
const RAIL_PREVIEW = 6;

/** One of the quotes behind an answer, drawn as the thing that was read rather than as a field: the source and when it was on one grey line, the words themselves on a sunken block under it. Input: the rows the daemon sent, and whether this is the rail's compact list. Output: the stack of them. In the compact form, more than RAIL_PREVIEW sources are folded behind a "show all N" control rather than all listed at once. */
export function Quotes({
  evidence,
  compact,
}: {
  evidence: Evidence[];
  compact?: boolean;
}) {
  const [all, setAll] = useState(false);
  const shown = compact && !all ? evidence.slice(0, RAIL_PREVIEW) : evidence;
  return (
    <div
      className={compact ? "flex flex-col gap-3" : "mt-3 flex flex-col gap-2"}
    >
      {keyed(shown, (e) => `${e.title}\n${e.meta}\n${e.body}`).map(({ key, item: e }) => (
        <div
          key={key}
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
      {compact && !all && evidence.length > RAIL_PREVIEW ? (
        <button
          type="button"
          onClick={() => setAll(true)}
          className="self-start rounded-sm text-meta text-muted-foreground underline-offset-2 outline-none hover:text-foreground hover:underline focus-visible:ring-2 focus-visible:ring-ring"
        >
          show all {evidence.length}
        </button>
      ) : null}
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
      // Only while the block is open: it is rendered only then, and a closed row naming an id that is not in the document is an invalid attribute value that offers a screen reader a jump to nothing.
      aria-controls={open ? controls : undefined}
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

/** What a failed ask says under it: the time and that it could not answer, with the provider's own message folded away when it says something the reason line above does not. Input: the turn and whether this reply folds at all. Output: the line and the fold. */
function ErrorMeta({ turn, folded }: { turn: Turn; folded: boolean }) {
  const dispatch = useAppDispatch();
  const open = useAppSelector((s) => s.ui.openRails.includes(turn.id));
  const text = (turn.text ?? "").trim();
  const whole = text.length > 0 && text !== (turn.reason ?? "").trim();
  const foldable = whole && folded;
  return (
    <>
      <div className="mt-1.5 text-meta text-muted-foreground">{hhmm(turn.when)} · could not answer</div>
      {foldable ? (
        <Fold label="The whole message" open={open} controls={`fold-${turn.id}`} onToggle={() => dispatch(ui.railToggled(turn.id))} />
      ) : null}
      {foldable && open ? (
        <pre id={`fold-${turn.id}`} className="mt-2 max-h-[220px] overflow-auto rounded-md bg-sunken p-3 font-mono text-meta whitespace-pre-wrap">
          {turn.text}
        </pre>
      ) : null}
    </>
  );
}

/** What an answer says under it: the time and the tools it called on one grey line, and the quotes behind it folded away. Input: the turn and whether this reply folds at all. Output: the line, the fold, and the quotes when the fold is open. An answer that read nothing says so, since a reply built on nothing is one to treat differently. */
function AnswerMeta({ turn, folded }: { turn: Turn; folded: boolean }) {
  const dispatch = useAppDispatch();
  const open = useAppSelector((s) => s.ui.openRails.includes(turn.id));
  const evidence = turn.evidence ?? [];
  const tools = (turn.tools ?? []).filter(Boolean);
  const foldable = evidence.length > 0 && folded;
  return (
    <>
      <div className="mt-1.5 text-meta text-muted-foreground">
        {hhmm(turn.when)}
        {tools.length && folded ? ` · ${tools.join(", ")}` : ""}
        {evidence.length ? "" : " · read nothing — treat it that way"}
      </div>
      {foldable ? (
        <Fold label="Sources" count={evidence.length} open={open} controls={`fold-${turn.id}`} onToggle={() => dispatch(ui.railToggled(turn.id))} />
      ) : null}
      {foldable && open ? (
        <div id={`fold-${turn.id}`}>
          <Quotes evidence={evidence} />
        </div>
      ) : null}
    </>
  );
}

/** What sits under one of Ora's replies: the grey line saying when it was said and which tools it called, and the fold holding what it read. Input: the turn. Output: the line and the fold. A failed ask gets the same shape, except that what the fold holds is the provider's whole message rather than what was read. */
export function ReplyMeta({ turn, folded = true }: { turn: Turn; folded?: boolean }) {
  return turn.kind === "error" ? <ErrorMeta turn={turn} folded={folded} /> : <AnswerMeta turn={turn} folded={folded} />;
}
