/** The small pieces more than one screen draws: the reading column every page sets its text in, the scrolling region that keeps its scrollbar out of the way, the bar across the top of a screen, what an empty list and an empty pane say, the heading that opens a section, the card a group of settings rows sits in, the picker in the header of Tasks, Meetings and Days, and the brain picker in the header of Chats and Tasks. The rules these follow are in DESIGN.md. */

import { Fragment, useCallback, useEffect, useLayoutEffect, useRef, useState, type ReactNode, type RefObject } from "react";
import { ChevronDown } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Command, CommandGroup, CommandInput, CommandItem, CommandList } from "@/components/ui/command";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuLabel, DropdownMenuSeparator, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { ScrollArea } from "@/components/ui/scroll-area";
import { SidebarTrigger } from "@/components/ui/sidebar";
import { Popover, PopoverContent, PopoverTrigger } from "./popover";
import { AUTOMATIC_BRAIN, useOpenUrlMutation, usePickBrainMutation, type Brain, type UsageLimit } from "./api";
import { atBottom, hhmm, windowLabel } from "./format";
import { Face } from "./face";
import { ui, useAppDispatch, useAppSelector, type Queries } from "./store";

/** The one reading column the whole window sets its text in: 68 characters at 15px, centred, with the same margins at every window width. A wider window gets wider margins rather than bigger text, which is why nothing here changes with a breakpoint. Defined in index.css so the rule lives with the other tokens. */
export const MEASURE = "measure";

/** The room under the last block of a scrolling page, so the final line is never hard against the bottom edge. 96px, the same on every screen. */
export const TAIL = "pb-24";

/** The room above the first block of a scrolling page. 32px, the same on every screen. */
export const HEAD = "pt-8";

/** The pane width past which a page stops being one narrow column in the middle of an empty window: the measure goes to 72 characters at 16px, and a rail appears beside the document carrying what was inside it or missing from it. 1500px, measured on the pane rather than on the window, because collapsing the sidebar changes the pane's width without changing the window's. */
export const WIDE = 1500;

/** Whether the pane this screen is drawn in is wide enough for the rail. Input: none. Output: the answer, and the ref to put on the screen's own root element so there is something to measure. Measured on every resize of that element, so dragging the window or collapsing the sidebar changes the layout without a reload. */
export function useWide(): [boolean, RefObject<HTMLDivElement | null>] {
  const pane = useRef<HTMLDivElement>(null);
  const [wide, setWide] = useState(false);

  useLayoutEffect(() => {
    const el = pane.current;
    if (!el) return;
    const check = () => setWide(el.getBoundingClientRect().width >= WIDE);
    check();
    const watch = new ResizeObserver(check);
    watch.observe(el);
    window.addEventListener("resize", check);
    return () => {
      watch.disconnect();
      window.removeEventListener("resize", check);
    };
  }, []);

  return [wide, pane];
}

/** Which of a page's marked blocks is the one being read, and how to go to another. Input: the ids of the blocks, in the order they appear; each must be the id of an element on the page. Output: the id of the last block whose top has passed the reading line — a fifth of the way down the scrolling region — and a function that scrolls one into view.
 *
 * There is no ref to pass because the elements are found by their ids and the region they scroll in is whichever scrolling ancestor they sit in, which is the ScrollArea viewport on every screen that uses this.
 */
export function useReading(ids: string[]): { active?: string; goTo: (id: string) => void } {
  const [active, setActive] = useState<string | undefined>(ids[0]);
  const key = ids.join("|");

  useEffect(() => {
    const list = key ? key.split("|") : [];
    if (!list.length) return;
    const first = document.getElementById(list[0]);
    const view = first?.closest("[data-slot=scroll-area-viewport]");
    if (!view) return;
    const check = () => {
      const line = view.getBoundingClientRect().top + view.clientHeight / 5;
      let seen = list[0];
      for (const id of list) {
        const el = document.getElementById(id);
        if (el && el.getBoundingClientRect().top <= line) seen = id;
      }
      setActive(seen);
    };
    check();
    view.addEventListener("scroll", check);
    window.addEventListener("resize", check);
    return () => {
      view.removeEventListener("scroll", check);
      window.removeEventListener("resize", check);
    };
  }, [key]);

  const goTo = useCallback((id: string) => document.getElementById(id)?.scrollIntoView({ block: "start", behavior: "smooth" }), []);
  return { active, goTo };
}

/** Keeps the keyboard's own focus on whichever row is selected, in a list where the arrow keys walk the selection through the store rather than through the row itself (App.tsx's `step`). Without this, pressing an arrow moves which row is highlighted but leaves the visible ring behind on the row that happened to be focused before — a mismatch a sighted keyboard user notices at once and a screen reader user is told nothing about, since nothing actually moved. Input: the list's own root element and the id selected now. Output: nothing; as a side effect, once a keypress changes which id is selected, the row carrying that id is focused in turn — but only when the keyboard was already inside this list, so a selection that changed for some other reason (a click elsewhere, a row being deleted) does not reach out and steal the focus of whatever the user is doing next. Each row must carry `data-row-id` set to its own id for this to find it. */
export function useFollowSelection(container: RefObject<HTMLElement | null>, id?: string) {
  useEffect(() => {
    if (!id) return;
    const root = container.current;
    if (!root || !root.contains(document.activeElement)) return;
    root.querySelector<HTMLElement>(`[data-row-id="${CSS.escape(id)}"]`)?.focus();
  }, [container, id]);
}

/** Whether this window runs on Windows, for the sentences that name the tray, the Start menu or Windows' own settings. */
export const ON_WINDOWS = typeof navigator !== "undefined" && /windows/i.test(navigator.userAgent);

/** Opens Settings scrolled to one of its sections, for the actions that fix a problem the window says somewhere else. Input: none. Output: the function, taking the section's id ("brain", "local-features"). */
export function useOpenSettingsAt(): (section: string) => void {
  const dispatch = useAppDispatch();
  return (section) => {
    dispatch(ui.placeShown("settings"));
    // Settings has to be drawn before there is anything to scroll to.
    setTimeout(() => document.getElementById(section)?.scrollIntoView({ block: "start" }), 50);
  };
}

/** Opens a link in the person's own browser. Input: none. Output: the function that opens one. The daemon's POST /open is what reliably reaches the browser from the desktop window, which is why the reply markdown's links use it too; a plain browser tab with no daemon route to ask — the mock page — opens a tab of its own instead. */
export function useOpenLink(): (href: string) => void {
  const [openUrl] = useOpenUrlMutation();
  return (href) => {
    void openUrl(href)
      .unwrap()
      .catch(() => window.open(href, "_blank", "noopener"));
  };
}

/** What an empty list says where there is room for one short line, with June's own face above it — the hero of the empty state, sized a step up from the page's own heading rather than left at the sidebar chip's size. Input: whether the daemon answered, the sentence for an answer with nothing in it, and whether the list is still on its first fetch. While loading, the real empty sentence is held back and a "thinking" face shows instead, so "Nothing to do." never flashes before the data it describes has actually arrived. Output: the block. */
export function Nothing({ up, empty, loading }: { up: boolean; empty: string; loading?: boolean }) {
  const refused = useAppSelector((s) => s.progress.refused);
  const state = loading ? "thinking" : up ? "watching" : refused ? "refused" : "asleep";
  const line = loading ? "Looking…" : up ? empty : refused ? "This window lost touch with June. Reopen it." : "Not connected.";
  return (
    <div className="flex flex-col items-center gap-2 px-2 py-6 text-center">
      <Face state={state} className="text-title" />
      <p className="text-ui text-muted-foreground">{line}</p>
    </div>
  );
}

/** What an empty pane says, which is the whole reading side of the window and so gets the four parts an empty state is made of: June's own face as its hero, one line in the text colour naming what is not there, one muted sentence saying what to do about it, and the action itself when there is one to offer. Input: whether the daemon answered, the line for an answer with nothing in it, the sentence under it, the action, and whether the pane is still on its first fetch. While loading, the hint and action are held back and the face shows "thinking" with a short "Looking…" in place of the real empty line, so the real copy never flashes before data arrives. A daemon that answered but refused this window's key (progress.refused) says that rather than that nothing is answering, since the fix is the opposite one: the daemon is up, and it is the window that needs the new key. Output: a centred block. */
export function Blank({ up, empty, hint, action, loading }: { up: boolean; empty: string; hint?: string; action?: ReactNode; loading?: boolean }) {
  const refused = useAppSelector((s) => s.progress.refused);
  const state = loading ? "thinking" : up ? "watching" : refused ? "refused" : "asleep";
  const title = loading ? "Looking…" : up ? empty : refused ? "This window lost touch with June" : "June isn't answering";
  const under = loading
    ? undefined
    : up
      ? hint
      : refused
        ? "June is running, but this window is out of date. Close it and open it again, or restart June."
        : "June isn't running right now. This window keeps trying and fills in once June is back.";
  return (
    <div className="m-auto flex max-w-[46ch] flex-col items-center gap-3 px-8 py-12 text-center">
      <Face state={state} className="text-title" />
      <p className="text-doc text-foreground">{title}</p>
      {under ? <p className="text-read text-muted-foreground">{under}</p> : null}
      {up && !loading && action ? <div className="mt-2">{action}</div> : null}
    </div>
  );
}

/** The heading that opens a section of a page. It is the same colour as the text under it and is told apart by weight and by the space above it — never small grey capitals, which read as a form's field label rather than as part of the page. Input: the words, and any control that belongs on the same line. Output: the heading. */
export function SectionHeading({ children, aside }: { children: ReactNode; aside?: ReactNode }) {
  return (
    <div className="mb-3 flex items-baseline justify-between gap-4">
      <h2 className="text-doc text-foreground">{children}</h2>
      {aside ? <div className="shrink-0 text-meta text-muted-foreground">{aside}</div> : null}
    </div>
  );
}

/** The card a group of related rows sits in, the way a desktop settings pane groups them: one surface, one hairline round it, and the rows inside told apart by an inset rule rather than by a gap. Input: the rows. Output: the card. */
export function Group({ children, className }: { children: ReactNode; className?: string }) {
  return <div className={`overflow-hidden rounded-lg border bg-card ${className ?? ""}`}>{children}</div>;
}

/** The body of a reading page: one column at the window's measure, and beside it, once the pane is wide enough, a rail carrying what belongs next to the document rather than inside it. Input: whether the pane is wide, the rail's contents, and the document. Output: the column, alone or with the rail beside it. A page that passes no rail gets the same single column at every width, which is what Tasks and Settings want. */
export function Reading({ wide, rail, children }: { wide: boolean; rail?: ReactNode; children: ReactNode }) {
  if (!wide) return <div className={MEASURE}>{children}</div>;
  // A page with nothing to put beside it still takes the wider measure, so a maximised window is not a narrow strip of a page with an inch of margin either side.
  if (!rail) return <div className="measure-wide px-10">{children}</div>;
  // A grid rather than a row, so the two tracks are exactly 72 characters and 280 pixels and the leftover space falls evenly on both sides instead of all of it landing to the right of the rail.
  return (
    <div className="reading-wide grid w-full justify-center gap-14 px-10 [grid-template-columns:minmax(0,72ch)_280px]">
      <div className="min-w-0">{children}</div>
      {rail}
    </div>
  );
}

/** The rail beside a document. Input: what it names itself to a screen reader and what it holds. Output: the column, 280px wide and pinned to the top of the region as the document scrolls past it, with a hairline down its left edge so it reads as a column next to the document rather than a second document floating beside it. Capped to the pane's own height, less the header's h-12, and scrolling within itself past that, so a reply with many sources grows inside the rail instead of over the thread beside it. */
export function Rail({ label, children }: { label: string; children: ReactNode }) {
  return (
    <aside aria-label={label} className="sticky top-0 flex max-h-[calc(100vh-3rem)] w-[280px] shrink-0 flex-col gap-7 self-start overflow-y-auto border-l border-hairline pl-6">
      {children}
    </aside>
  );
}

/** One block of the rail: a quiet heading and whatever sits under it. Input: the heading and the block. Output: the pair. The heading here is the one place small type is allowed to label something, because the rail is a margin note and not part of the document. */
export function RailBlock({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section>
      <h3 className="mb-2 text-meta font-medium text-muted-foreground">{title}</h3>
      <div className="text-meta text-muted-foreground">{children}</div>
    </section>
  );
}

/** The document's own headings as a way into it. Input: the sections, each with the id of its heading on the page and its words, and the id of the one being read now, plus what to do when one is clicked. Output: the list, with the section in view marked by a solid rule and the text colour. */
export function Outline({ sections, active, onPick }: { sections: { id: string; text: string }[]; active?: string; onPick: (id: string) => void }) {
  if (sections.length < 2) return null;
  return (
    <nav aria-label="Sections" className="flex flex-col border-l">
      {sections.map((sec) => (
        <button
          key={sec.id}
          type="button"
          aria-current={sec.id === active ? "true" : undefined}
          onClick={() => onPick(sec.id)}
          title={sec.text}
          className={`-ml-px truncate border-l-2 py-1 pl-3 text-left text-meta outline-none transition-colors focus-visible:ring-2 focus-visible:ring-ring ${sec.id === active ? "border-primary text-foreground" : "border-transparent text-muted-foreground hover:text-foreground"}`}
        >
          {sec.text}
        </button>
      ))}
    </nav>
  );
}

/** The bar across the top of every screen. The rail's own toggle stays against the window edge where it belongs, and everything else in the header — the screen's name, its picker, the brain — sits in the same column the page below it reads in, so the picker lines up with the first word under it rather than with the far edge of a maximised window.
 *
 * It keeps that promise by being laid out with Reading, the same component the page under it uses, rather than by naming a width of its own: at a wide pane with a rail beside the page the header reserves the rail's width too, so all three of the header, the document and the composer share one pair of edges.
 *
 * Input: what the header holds, whether the pane is wide, and whether the page under it has a rail. Output: the bar.
 */
export function PageHeader({ children, wide = false, railed = false, full = false }: { children: ReactNode; wide?: boolean; railed?: boolean; full?: boolean }) {
  const row = <div className="flex min-w-0 items-center gap-1">{children}</div>;
  return (
    <header className="relative flex h-12 shrink-0 items-center border-b">
      <SidebarTrigger className="absolute left-2 z-10 text-muted-foreground" />
      {/* A page laid out as two columns rather than one reading column, Tasks, has nothing for the header to line up with but the pane's own edges. */}
      {full ? <div className="w-full pl-12 pr-6">{row}</div> : <Reading wide={wide} rail={wide && railed ? <div aria-hidden /> : undefined}>{row}</Reading>}
    </header>
  );
}

/** A scrolling region whose scrollbar is out of the page until something is scrolled, with a fade along the bottom edge for as long as there is more below. The fade is what a hidden scrollbar would otherwise cost: with nothing drawn down the side, a page that continues past the fold looks finished, and the fade says it is not. Input: what to scroll, any classes for the region, and any for the block inside it that the page's own layout needs. Output: the region. */
export function Scroller({ children, className, bodyClassName, newest }: { children: ReactNode; className?: string; bodyClassName?: string; newest?: string }) {
  const inner = useRef<HTMLDivElement>(null);
  const [more, setMore] = useState(false);
  // Whether the reader is at the end of what is here. It starts true so a thread opens showing its newest turn, and a scroll away from the end turns it off until they come back.
  const following = useRef(true);
  // How tall the region was last time it was pulled to the bottom, so a render that added nothing to the thread — the rail beside it picking a new reply to show, say — is not mistaken for a new turn arriving.
  const lastHeight = useRef(0);

  // A different thread is a fresh page, so it opens at its newest turn however far up the last one had been read, measured against nothing yet.
  useLayoutEffect(() => {
    following.current = true;
    lastHeight.current = 0;
  }, [newest]);

  // After every render, because what is in the region changes when the daemon answers and again with every word it streams. Laid out before the browser paints, so the thread is never seen jumping. Only when the region has actually grown since the last check, so a render that leaves the thread's own height untouched — the active reply the rail is showing changing, for one — cannot undo a reader who has scrolled away from the end but is still within the 120px band that would otherwise pull them back for a real new turn.
  useLayoutEffect(() => {
    if (!newest) return;
    const view = inner.current?.closest("[data-slot=scroll-area-viewport]");
    if (!view) return;
    const grew = view.scrollHeight > lastHeight.current;
    lastHeight.current = view.scrollHeight;
    if (!grew || !following.current) return;
    view.scrollTop = view.scrollHeight - view.clientHeight;
  });

  // Measured after every render rather than on a timer: what is in the region changes when the daemon answers, and the answer is the thing that decides whether there is more below.
  useEffect(() => {
    const view = inner.current?.closest("[data-slot=scroll-area-viewport]");
    if (!view) return;
    const check = () => {
      setMore(view.scrollHeight - view.scrollTop - view.clientHeight > 8);
      following.current = atBottom(view);
    };
    check();
    view.addEventListener("scroll", check);
    window.addEventListener("resize", check);
    return () => {
      view.removeEventListener("scroll", check);
      window.removeEventListener("resize", check);
    };
  });

  return (
    // Radix wraps what a viewport holds in a table box of automatic height, which a percentage inside it has nothing to resolve against. Making that box a column at least as tall as the viewport, and the block inside it the thing that fills the column, is what lets a short thread sit against the composer while a long page still scrolls.
    <ScrollArea
      type="scroll"
      className={`min-h-0 flex-1 [&>[data-radix-scroll-area-viewport]>div]:flex! [&>[data-radix-scroll-area-viewport]>div]:min-h-full [&>[data-radix-scroll-area-viewport]>div]:flex-col ${className ?? ""}`}
    >
      <div ref={inner} className={`flex-1 ${bodyClassName ?? ""}`}>
        {children}
      </div>
      {more ? <div aria-hidden className="pointer-events-none absolute inset-x-0 bottom-0 h-16 bg-linear-to-t from-background via-background/70 to-transparent" /> : null}
    </ScrollArea>
  );
}

/** One row a picker offers. id is what the screen opens when it is chosen, label is the row's first line, hint the grey one beside it, and group the heading it sits under. */
export type PickerOption = { id: string; label: string; hint?: string; group: string };

/** The one control Tasks, Meetings and Days each have in their header, in place of the inner rail those screens used to carry: a button naming what is open, and a popover holding a search field and everything that matches it. The same component on all three screens, so the search field, the headings, the keyboard walk and the empty line are one behaviour rather than three.
 *
 * Input: which list's search field this is, the sentence that names the picker to a screen reader, the placeholder on the field, the rows to offer already filtered by that search, the id showing now, and the line to say when the search matches nothing. Output: the control. What is typed goes into the store, so the page behind the popover narrows with it; closing the picker clears it again, which is what stops a filter surviving out of sight of the field that set it.
 */
export function Picker({
  list,
  label,
  placeholder,
  options,
  selected,
  empty,
  onPick,
}: {
  list: keyof Queries;
  label: string;
  placeholder: string;
  options: PickerOption[];
  selected?: string;
  empty: string;
  onPick: (id: string) => void;
}) {
  const dispatch = useAppDispatch();
  const query = useAppSelector((s) => s.ui.query[list]);
  const [open, setOpen] = useState(false);
  const current = options.find((o) => o.id === selected);

  const groups: { label: string; items: PickerOption[] }[] = [];
  for (const option of options) {
    const found = groups.find((g) => g.label === option.group);
    if (found) found.items.push(option);
    else groups.push({ label: option.group, items: [option] });
  }

  const close = (next: boolean) => {
    setOpen(next);
    if (!next) dispatch(ui.searched({ list, text: "" }));
  };

  return (
    <Popover open={open} onOpenChange={close}>
      <PopoverTrigger asChild>
        <Button variant="ghost" size="sm" className="min-w-0 max-w-[42ch] gap-1 px-1.5 font-normal text-muted-foreground hover:text-foreground">
          <span className="sr-only">{label}</span>
          <span className="truncate text-foreground" title={current?.label}>
            {current?.label ?? "nothing open"}
          </span>
          <ChevronDown className="shrink-0 opacity-60" />
        </Button>
      </PopoverTrigger>
      <PopoverContent className="w-[min(30rem,calc(100vw-3rem))] p-0">
        {/* The rows are already the ones the search left, so cmdk is told not to filter them again; one search, run in one place, and the page behind the popover narrows by the same call. */}
        <Command shouldFilter={false} label={label}>
          <CommandInput value={query} placeholder={placeholder} aria-label={placeholder} onValueChange={(text) => dispatch(ui.searched({ list, text }))} />
          <CommandList>
            {options.length === 0 ? (
              <p className="px-3 py-8 text-center text-ui text-muted-foreground">{empty}</p>
            ) : (
              groups.map((g) => (
                <CommandGroup key={g.label} heading={g.label}>
                  {g.items.map((o) => (
                    <CommandItem
                      key={o.id}
                      value={o.id}
                      data-checked={o.id === selected}
                      onSelect={() => {
                        onPick(o.id);
                        close(false);
                      }}
                    >
                      <span className={`min-w-0 flex-1 truncate ${o.id === selected ? "font-medium" : ""}`} title={o.label}>
                        {o.label}
                      </span>
                      {o.hint ? (
                        <span className="shrink-0 text-meta text-muted-foreground" title={o.hint}>
                          {o.hint}
                        </span>
                      ) : null}
                    </CommandItem>
                  ))}
                </CommandGroup>
              ))
            )}
          </CommandList>
        </Command>
      </PopoverContent>
    </Popover>
  );
}

/** "Resets in 3 hr 3 min" for a window that turns over within a day, rounded to the minute; "Resets Mon 18:33" past that, naming the weekday it falls on since a person is not tracking which day it is by then. Input: the limit's own resets_at (RFC3339) and the moment to measure it from. Output: the phrase, or "" when resets_at does not parse. */
function resetLabel(resetsAt: string, now: Date): string {
  const at = new Date(resetsAt);
  if (Number.isNaN(at.getTime())) return "";
  const ms = at.getTime() - now.getTime();
  if (ms >= 86400000) return `Resets ${at.toLocaleDateString(undefined, { weekday: "short" })} ${hhmm(resetsAt)}`;
  const mins = Math.max(0, Math.round(ms / 60000));
  const h = Math.floor(mins / 60);
  const m = mins % 60;
  return h > 0 ? `Resets in ${h} hr ${m} min` : `Resets in ${m} min`;
}

/** One allowance window drawn as a row under a brain's section in the picker, the way Claude Code's own usage widget lays one out: the window's name on the left, when it resets on the right — with the percent used tacked on while the reset is still close enough to say relatively — and underneath, spanning the row, a 3px bar in the accent colour that turns the danger colour once used_fraction reaches 0.9. Input: the limit, and the moment "resets in" is measured from (now, always, except in a test). Output: the row. */
export function UsageBar({ limit, now = new Date() }: { limit: UsageLimit; now?: Date }) {
  const pct = Math.round(limit.used_fraction * 100);
  const hot = limit.used_fraction >= 0.9;
  const reset = resetLabel(limit.resets_at, now);
  const right = reset.startsWith("Resets in") ? `${reset} · ${pct}%` : reset;
  return (
    <div className="flex flex-col gap-1">
      <div className="flex items-baseline justify-between gap-3 text-meta text-muted-foreground">
        <span>{windowLabel(limit.window)}</span>
        <span title={`${pct}%`}>{right}</span>
      </div>
      <div className="h-[3px] w-full overflow-hidden rounded-full bg-muted">
        <div className={`h-full rounded-full ${hot ? "bg-destructive" : "bg-primary"}`} style={{ width: `${Math.min(100, Math.max(0, pct))}%` }} />
      </div>
    </div>
  );
}

/** The muted line's text when a brain reports no limits but the daemon gave a reason: everything before the note's first colon, sentence-cased, so "grok exposes no usage data: ..." reads as "Grok exposes no usage data" while the full sentence stays on the row's title. A note with no colon is used whole. Input: the note, "" when the daemon sent none. Output: the clause, or "" when there is no note. */
function noteClause(note: string): string {
  if (!note) return "";
  const clause = note.split(":")[0].trim();
  return clause.charAt(0).toUpperCase() + clause.slice(1);
}

/** Where each brain's lapsed login is signed back into, by the name a person knows it under: the daemon's own notes name a terminal command ("run codex login"), which nobody who installed June from a setup program should be sent to. */
const SIGN_IN_APP: Record<string, string> = {
  codex: "the Codex app",
  claude: "Claude Code",
  antigravity: "Antigravity",
  grok: "Grok",
};

/** The line under a brain that is not signed in. Input: the row. Output: "Open {app} and sign in again" for a login its provider refused — the same reading onboarding's loginsFound makes of the note — and otherwise the note itself, unless it names a terminal or a command, in which case just "Not signed in". Not even on a title: a tooltip naming a terminal command is still the window telling someone to open a terminal. */
function signedOutLine(b: Brain): string {
  const note = b.limits_note ?? "";
  if (/expired|refused|sign in again|log in again/i.test(note)) return `Open ${SIGN_IN_APP[b.id] ?? b.name} and sign in again`;
  if (!note || /terminal|\brun\b|_API_KEY|\.env\b|config/i.test(note)) return "Not signed in";
  return note.charAt(0).toUpperCase() + note.slice(1);
}

/** The brain control in the header of Chats and Tasks, and on Settings: which backend answers this conversation, and the list to pick another from. Input: the brain the conversation names, which is "" when it was opened without one; every brain the daemon reported; and whether no brain is pinned and the daemon's router picks one per question. Output: the control. A brain that is not signed in is shown greyed and cannot be picked; picking one writes the choice through POST /brains, which is what makes it the daemon's default rather than something this window remembers, and "Automatic" at the top of the list is the way back to letting June pick. */
export function BrainPicker({ current, brains, automatic = false }: { current: string; brains: Brain[]; automatic?: boolean }) {
  const dispatch = useAppDispatch();
  const [pickBrain] = usePickBrainMutation();
  // A conversation the daemon opened without naming a brain is answered by whatever the daemon is set to: its router's own choice when nothing is pinned, and otherwise the one GET /brains marks default, which the header names rather than the word "default".
  const named = current ? brains.find((b) => b.id === current || b.name === current) : undefined;
  const auto = !current && automatic;
  const chosen = named ?? (current || auto ? undefined : brains.find((b) => b.default));

  /** Picks a brain, or with none hands the choice back to the daemon's router. The brain's own model goes with it as it stands, "" included: filling in the first listed model pinned one nobody had picked and wrote it into june-config.json. */
  const pick = async (b?: Brain) => {
    try {
      await pickBrain(b ? { brain: b.id, model: b.model } : { brain: AUTOMATIC_BRAIN }).unwrap();
    } catch {
      dispatch(ui.noticed({ text: "Could not change the brain", kind: "error" }));
    }
  };

  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="ghost" size="sm" className="gap-1 px-1.5 font-normal text-muted-foreground hover:text-foreground">
          Brain: <span className="text-foreground">{auto ? "Automatic" : chosen ? chosen.name : current || "not set"}</span>
          <ChevronDown className="opacity-60" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="w-[340px]">
        {brains.length === 0 ? (
          <DropdownMenuLabel className="font-normal text-muted-foreground">No brains reported</DropdownMenuLabel>
        ) : (
          <>
            <DropdownMenuItem onClick={() => void pick()} className="flex-col items-stretch gap-1 py-2">
              <span className={auto ? "font-medium" : undefined}>Automatic — June picks</span>
              <p className="text-meta text-muted-foreground">A signed-in brain for each question, none pinned</p>
            </DropdownMenuItem>
            {brains.map((b) => (
              <Fragment key={b.id}>
                <DropdownMenuSeparator />
                <DropdownMenuItem disabled={!b.signed_in} onClick={() => void pick(b)} className="flex-col items-stretch gap-2 py-2">
                  <div className="flex items-center justify-between gap-3">
                    <span className={chosen && b.id === chosen.id ? "font-medium" : undefined}>{b.name}</span>
                    <span className="text-meta text-muted-foreground">{b.signed_in ? b.model || "default model" : "not signed in"}</span>
                  </div>
                  {/* A brain that cannot be called has no allowance worth drawing — Gemini with no key still reports an untouched daily window — and what it does have to say is usually how to sign in. */}
                  {!b.signed_in ? (
                    <p className="text-meta text-muted-foreground">{signedOutLine(b)}</p>
                  ) : b.limits && b.limits.length > 0 ? (
                    <div className="flex flex-col gap-2">
                      {b.limits.map((l, j) => (
                        <UsageBar key={`${j}-${l.window}`} limit={l} />
                      ))}
                    </div>
                  ) : b.limits_note ? (
                    <p className="text-meta text-muted-foreground" title={b.limits_note}>
                      {noteClause(b.limits_note)}
                    </p>
                  ) : (
                    <p className="text-meta text-muted-foreground">No usage data</p>
                  )}
                </DropdownMenuItem>
              </Fragment>
            ))}
          </>
        )}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
