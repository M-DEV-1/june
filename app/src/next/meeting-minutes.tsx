/** A meeting's minutes, typeset as a document: headings, real lists and paragraphs, not a column of grey labels and not with their "##" and "**" showing. */

import type { MinutesLine } from "./format";

/** One block of the minutes as it is drawn: a heading, a paragraph, a label naming the list beneath it, or a run of bullets gathered into one list. */
type MinutesBlock = { kind: "h" | "label" | "text"; text: string; lead?: string } | { kind: "list"; items: MinutesLine[] };

/** Gathers the lines the minutes reader produced into the blocks a document is made of, so a run of bullets becomes one list rather than a paragraph each. Input: the lines. Output: headings and paragraphs as they came, and each run of bullets as one list. */
export function minutesBlocks(lines: MinutesLine[]): MinutesBlock[] {
  const out: MinutesBlock[] = [];
  for (const line of lines) {
    const last = out[out.length - 1];
    if (line.kind === "bullet") {
      if (last && last.kind === "list") last.items.push(line);
      else out.push({ kind: "list", items: [line] });
      continue;
    }
    out.push(line.lead ? { kind: line.kind, text: line.text, lead: line.lead } : { kind: line.kind, text: line.text });
  }
  return out;
}

/** The id the outline scrolls a section by. Input: which heading it is. Output: the id, unique on the page. */
export function sectionId(at: number): string {
  return `minutes-section-${at}`;
}

/** One meeting's minutes as they read on the page. Input: the blocks, already gathered. Output: the headings, lists and paragraphs, typeset by the .document rules in index.css. A line's lead phrase is set in medium weight ahead of its text rather than left running into the sentence, and a label is drawn with no marker above the list it names. */
export function Minutes({ blocks }: { blocks: MinutesBlock[] }) {
  if (!blocks.length) return <p className="text-muted-foreground">This recording has no minutes.</p>;
  let heading = -1;
  return (
    <div className="document">
      {blocks.map((b, i) => {
        if (b.kind === "h") {
          heading += 1;
          return (
            <h3 key={i} id={sectionId(heading)}>
              {b.text}
            </h3>
          );
        }
        if (b.kind === "list") {
          return (
            <ul key={i}>
              {b.items.map((item, j) => (
                <li key={j}>
                  {item.lead ? <span className="lead">{item.lead} — </span> : null}
                  {item.text}
                </li>
              ))}
            </ul>
          );
        }
        return (
          <p key={i} className={b.kind === "label" ? "label" : undefined}>
            {b.lead ? <span className="lead">{b.lead} — </span> : null}
            {b.text}
          </p>
        );
      })}
    </div>
  );
}
