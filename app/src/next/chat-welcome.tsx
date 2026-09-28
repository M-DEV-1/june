/** The suggestions a new chat opens with, under the greeting: a few things Ora can actually do, in place of a line telling the person about the furniture.
 *
 * Only the chips live here. The face, the greeting and the sentence under it are Blank's own four-part empty state (see parts.tsx), which every other screen's empty pane already uses, so the front door reads as the same window rather than a page of its own.
 * A suggestion fills the composer instead of sending, so it is a starting point the person can edit rather than a button committing them to a phrasing Ora chose.
 */

import { DRAFT_CHAT, ui, useAppDispatch } from "./store";

/** The suggestions, in the order they read. Each is a sentence the user would plausibly have typed, so one dropped in the box can be sent as it stands.
 * The last carries the "do:" prefix that opens a computer-use job rather than an ask (see the submit case in store.ts), and is left unfinished on purpose: a task needs saying what it is, and the prefix is the part nobody should have to remember.
 */
const SUGGESTIONS: { text: string; label?: string }[] = [
  { text: "Summarise my day" },
  { text: "What was I working on yesterday?" },
  { text: "What do I still owe anyone?" },
  { text: "do: ", label: "Run a task" },
];

/** Input: none. Output: the chips, as a list, since that is what they are. */
export function ChatSuggestions() {
  const dispatch = useAppDispatch();
  return (
    <ul className="flex flex-wrap justify-center gap-2">
      {SUGGESTIONS.map(({ text, label }) => (
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
