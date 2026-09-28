/** A meeting's minutes, typeset as a document: headings, real lists and paragraphs, not a column of grey labels and not with their "##" and "**" showing. */

import type { MinutesBlock } from "./format";

/** One meeting's minutes as they read on the page. Input: the blocks, already gathered. Output: the headings, lists and paragraphs, typeset by the .document rules in index.css. A line's lead phrase is set in medium weight ahead of its text rather than left running into the sentence, and a label is drawn with no marker above the list it names. */
export function Minutes({ blocks }: { blocks: MinutesBlock[] }) {
  if (!blocks.length) return <p className="text-muted-foreground">This recording has no minutes.</p>;
  return (
    <div className="document">
      {blocks.map((b) => {
        if (b.kind === "h") {
          // The block's own id is the anchor the outline scrolls to, so the page and the rail name a heading the same way without either of them counting headings.
          return (
            <h3 key={b.id} id={b.id}>
              {b.text}
            </h3>
          );
        }
        if (b.kind === "list") {
          return (
            <ul key={b.id}>
              {b.items.map((item) => (
                <li key={item.id}>
                  {item.lead ? <span className="lead">{item.lead} — </span> : null}
                  {item.text}
                </li>
              ))}
            </ul>
          );
        }
        return (
          <p key={b.id} className={b.kind === "label" ? "label" : undefined}>
            {b.lead ? <span className="lead">{b.lead} — </span> : null}
            {b.text}
          </p>
        );
      })}
    </div>
  );
}
