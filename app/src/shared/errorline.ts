/** The one line a failed message reads as. Input: the raw text, which may be a provider's whole error, and cap, how many characters of it are shown before the rest is folded away (default 150, since a provider's own error can run past a thousand characters and this is what stops one failure filling the answer slot). Output: line, its first line cut at cap characters on a word boundary with an ellipsis, and more, whether anything was left out of the line. The hover's errorLine (state.ts) and the window's turnText (next/format.ts) both cut through this. */
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
