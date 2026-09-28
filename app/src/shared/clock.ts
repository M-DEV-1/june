/** The one way both windows write a clock time: 24-hour, to the minute, zero-padded ("09:05"). Input: the moment. Output: the label. */
export function clockTime(d: Date): string {
  return `${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`;
}
