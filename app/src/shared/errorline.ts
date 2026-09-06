/** The word-boundary truncation both windows use for a failed ask's message: the hover's state.ts wants the cut text handed back as `detail`, next/format.ts only wants to know whether anything was cut, as `more`. This returns the `more` shape, since that is what format.ts already returns untouched, and it is also everything state.ts needs to build its own `detail` field from. */

/** The one line a failed message reads as. Input: the raw text, which may be a provider's whole error, and cap, how many characters of it are shown before the rest is folded away (default 150, since a provider's own error can run past a thousand characters and this is what stops one failure filling the answer slot). Output: line, its first line cut at cap characters on a word boundary with an ellipsis, and more, whether anything was left out of the line. */
export function truncateAtWord(
  text: string,
  cap: number = 150,
): { line: string; more: boolean } {
  const whole = (text ?? "").trim();
  const first = whole.split("\n")[0].trim();
  if (first === whole && first.length <= cap) return { line: first, more: false };
  if (first.length <= cap) return { line: first, more: true };
  const cut = first.slice(0, cap);
  const space = cut.lastIndexOf(" ");
  return { line: `${(space > 40 ? cut.slice(0, space) : cut).trimEnd()}…`, more: true };
}
