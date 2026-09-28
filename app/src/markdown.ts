/** The hover window's own markdown, for the replies June writes back. The main window renders these through react-markdown; this window is a small always-on surface that does not carry React, so it gets a renderer covering what a reply actually contains — paragraphs, bullet and numbered lists, bold, italic and inline code — and nothing else. Everything is escaped before any tag is put back, exactly as the plain escape does, so nothing a model wrote or a tool read off the screen can turn into markup. */

/** Escapes the four characters that could otherwise open a tag or close an attribute. Input: raw text. Output: text safe to place in HTML, both in a text node and inside a double-quoted attribute. The hover's templates in main.ts use this same call for questions, titles and aria-labels. */
export function esc(s: string): string {
  return s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}

/** The inline marks, applied to already-escaped text: **bold**, *italic* or _italic_, `code`, and the two literal tags an answer is allowed to carry through from the daemon. Input: escaped text. Output: the same text with those spans marked up. */
function inline(s: string): string {
  return s
    .replace(/`([^`]+)`/g, "<code>$1</code>")
    .replace(/\*\*([^*]+)\*\*/g, "<b>$1</b>")
    .replace(/(^|[\s(])\*([^*\n]+)\*(?=[\s.,;:!?)]|$)/g, "$1<i>$2</i>")
    .replace(/(^|[\s(])_([^_\n]+)_(?=[\s.,;:!?)]|$)/g, "$1<i>$2</i>")
    .replace(/&lt;(\/?)(b|mark)&gt;/g, "<$1$2>");
}

/** What a line looks like when it opens a list item: a dash, star or plus for a bullet, a number and a dot or bracket for an ordered one. */
const BULLET = /^\s*[-*+]\s+(.*)$/;
const NUMBER = /^\s*\d+[.)]\s+(.*)$/;

/** Renders one block — a run of lines with no blank line in it — as a list when every line opens an item, and as a paragraph otherwise. Input: the block's lines, already escaped. Output: the block's HTML. */
function block(lines: string[]): string {
  const bullets = lines.every((l) => BULLET.test(l));
  const numbers = lines.every((l) => NUMBER.test(l));
  if (bullets || numbers) {
    const pattern = bullets ? BULLET : NUMBER;
    const items = lines.map((l) => `<li>${inline(l.replace(pattern, "$1"))}</li>`).join("");
    return bullets ? `<ul>${items}</ul>` : `<ol>${items}</ol>`;
  }
  // A single newline inside a paragraph is a line break, not a new paragraph: a model wrapping a sentence should not open a new block.
  return `<p>${inline(lines.join("\n")).replace(/\n/g, "<br>")}</p>`;
}

/** Renders a reply as HTML. Input: the reply text, exactly as the daemon sent it. Output: HTML safe to assign, or "" for empty text. */
export function markdown(s: string): string {
  const text = s.trim();
  if (text === "") return "";
  const out: string[] = [];
  let run: string[] = [];
  const flush = (): void => {
    if (run.length > 0) out.push(block(run));
    run = [];
  };
  for (const raw of esc(text).split("\n")) {
    const line = raw.trimEnd();
    if (line.trim() === "") {
      flush();
      continue;
    }
    // A list item always starts its own block, so a paragraph running straight into "- one" does not swallow the list.
    const opensItem = BULLET.test(line) || NUMBER.test(line);
    const inList = run.length > 0 && (BULLET.test(run[0]) || NUMBER.test(run[0]));
    if (opensItem !== inList) flush();
    run.push(line);
  }
  flush();
  return out.join("");
}
