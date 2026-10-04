/** The suggestions a new chat opens with, under the greeting: a few things June can actually do, in place of a line telling the person about the furniture.
 *
 * Only the chips live here. The face, the greeting and the sentence under it are Blank's own four-part empty state (see parts.tsx), which every other screen's empty pane already uses, so the front door reads as the same window rather than a page of its own.
 * A suggestion fills the composer instead of sending, so it is a starting point the person can edit rather than a button committing them to a phrasing June chose.
 */

import { useDaysQuery } from "./api";
import { activeDays, isoDay } from "./format";
import { DRAFT_CHAT, ui, useAppDispatch } from "./store";

type Suggestion = { text: string; label?: string };

/** The suggestions once June has a day behind it, in the order they read. Each is a sentence the user would plausibly have typed, so one dropped in the box can be sent as it stands.
 * The last carries the "do:" prefix that opens a computer-use job rather than an ask (see the submit case in store.ts), and is left unfinished on purpose: a task needs saying what it is, and the prefix is the part nobody should have to remember.
 */
const SUGGESTIONS: Suggestion[] = [
  { text: "Summarise my day" },
  { text: "What was I working on yesterday?" },
  { text: "What do I still owe anyone?" },
  { text: "do: ", label: "Run a task" },
];

/** The suggestions on June's first day, every one of which works with nothing remembered yet: what is on screen now, a task to keep, and a job to run. On day one "yesterday" came back empty and read as June knowing nothing. */
const FIRST_DAY: Suggestion[] = [
  { text: "What's on my screen right now?" },
  { text: "Add to my tasks: ", label: "Add a task" },
  { text: "do: ", label: "Run a task" },
];

/** What the front door says under the greeting on June's first day, so an empty answer about the past is expected rather than a surprise. */
export const FIRST_DAY_HINT = "I've only just started keeping notes, so ask about what's on your screen now. After a day of use I can tell you what you worked on and what you still owe.";

/** Whether June has nothing from before today to answer from. Input: none. Output: true until GET /days lists a day before today with anything in it; false while the list has not come back, so the usual front door is what a slow daemon shows. */
export function useFirstDay(): boolean {
  const { data: days } = useDaysQuery();
  if (!days) return false;
  const today = isoDay(new Date());
  return !activeDays(days).some((d) => d.date < today);
}

/** Input: none. Output: the chips, as a list, since that is what they are. */
export function ChatSuggestions() {
  const dispatch = useAppDispatch();
  const list = useFirstDay() ? FIRST_DAY : SUGGESTIONS;
  return (
    <ul className="flex flex-wrap justify-center gap-2">
      {list.map(({ text, label }) => (
        <li key={text}>
          <button
            type="button"
            onClick={() => dispatch(ui.asked({ conversationId: DRAFT_CHAT, text }))}
            className="rounded-sm border border-hairline-strong bg-card px-3 py-1.5 text-meta text-foreground transition-colors hover:bg-hover focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          >
            {label ?? text}
          </button>
        </li>
      ))}
    </ul>
  );
}
