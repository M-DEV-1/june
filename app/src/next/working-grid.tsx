/** The one shape that means "Ora is busy": a row of the braille dot grid, the same signature the voice modes draw, muted and drawn from the clock because a run in flight has nothing to measure. */

import { useEffect, useState } from "react";

import { workingRow } from "../waveform";

/** How often the row is redrawn, in milliseconds. The wave takes WORKING_PERIOD_MS to travel once, so ten frames a second is smooth without being work. */
const TICK_MS = 100;

/** Says whether the reader asked the system for less motion. Input: none. Output: true when prefers-reduced-motion is reduce, and false when the page cannot answer, which is what jsdom does. */
function reducedMotion(): boolean {
  return (
    typeof window !== "undefined" &&
    typeof window.matchMedia === "function" &&
    window.matchMedia("(prefers-reduced-motion: reduce)").matches
  );
}

/** One row of braille dots saying Ora is working, redrawn ten times a second, or held on a single frame for a reader who asked for less motion. Input: the row's width in cells, 24 by default. Output: the row as one span, labelled for a screen reader as an image so its characters are not read out one by one. */
export function WorkingGrid({ width = 24 }: { width?: number }) {
  const still = reducedMotion();
  const [t, setT] = useState(() => (still ? 0 : Date.now()));
  useEffect(() => {
    if (still) return;
    const id = setInterval(() => setT(Date.now()), TICK_MS);
    return () => clearInterval(id);
  }, [still]);
  return (
    <span
      aria-label="Ora is working"
      role="img"
      className="font-mono text-meta text-muted-foreground select-none"
    >
      {workingRow(width, t)}
    </span>
  );
}
