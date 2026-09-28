/** Holds a value between two bounds, shared by the overlay's CSS-pixel math and the window placement's physical-pixel math. Input: the value, the low bound, the high bound, and whether to round the result (default false, which is what the overlay wants since it keeps sub-pixel precision). Output: the value moved inside the bounds, rounded when asked; when the bounds have crossed (high below low) this returns low, rounded or not, which is what a label wider than its own monitor or a window bigger than its work area both want. */
export function clamp(value: number, low: number, high: number, round = false): number {
  const out = Math.max(low, Math.min(high, value));
  return round ? Math.round(out) : out;
}
