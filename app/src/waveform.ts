/** Live audio-level waveform for the hover's voice session, ported from internal/ui/waveform.go so the desktop window draws the same braille bar the terminal client already has. The hover runs in a separate process from the daemon's microphone and speaker, so it never reads an amplitude itself — the daemon samples both every 50ms while a live session runs and ships {"mic":0-1,"speaker":0-1} as the "level" event's Detail (see internal/ipc/voice.go). This file turns one such reading into the braille rows a Waveform instance draws for one of the two channels. No DOM, no timers, nothing time-based beyond what the caller feeds in: update(amp) advances the smoothing, render() reads the eased value back out as strings. */

// riseAlpha: how fast amplitude rises (0=no rise, 1=instant).
// fallAlpha: how fast amplitude falls — 0.26 reaches near-zero in ~500ms (clear pauses, not jumpy).
const RISE_ALPHA = 0.85;
const FALL_ALPHA = 0.26;
const AMP_THRESHOLD = 0.01;

// row0Levels: braille masks for the row above the centre line, filling from its own bottom edge (the edge touching centre) outward as the level rises from 0 (empty) to 4 (full block).
const ROW0_LEVELS = [
  0x2800, // 0: empty
  0x2800 | 0x40 | 0x80, // 1: dots 7,8 (bottom of the char = centre)
  0x2800 | 0x40 | 0x80 | 0x04 | 0x20, // 2: + dots 3,6
  0x2800 | 0x40 | 0x80 | 0x04 | 0x20 | 0x02 | 0x10, // 3: + dots 2,5
  0x28ff, // 4: all dots
];

// row1Levels: braille masks for the row below the centre line, filling from its own top edge (the edge touching centre) outward.
const ROW1_LEVELS = [
  0x2800, // 0: empty
  0x2800 | 0x01 | 0x08, // 1: dots 1,4 (top of the char = centre)
  0x2800 | 0x01 | 0x08 | 0x02 | 0x10, // 2: + dots 2,5
  0x2800 | 0x01 | 0x08 | 0x02 | 0x10 | 0x04 | 0x20, // 3: + dots 3,6
  0x28ff, // 4: all dots
];

/** Generates the per-column multipliers waveform.go's buildVariation computes, using three overlapping sine waves so adjacent columns come out correlated but irregular rather than a flat block. Input: how many columns wide the bar is. Output: one multiplier per column, each in [0.35, 1.0]. */
export function buildVariation(width: number): number[] {
  const v = new Array<number>(width);
  for (let i = 0; i < width; i++) {
    const raw = Math.sin(i * 0.9 + 0.4) * 0.5 + Math.sin(i * 0.3 + 1.1) * 0.3 + Math.sin(i * 1.7) * 0.2;
    // raw is in ~[-1,1]; map to [0.35, 1.0].
    v[i] = 0.35 + (0.65 * (raw + 1.0)) / 2.0;
  }
  return v;
}

/**
 * Waveform smooths one audio channel's amplitude readings and renders them as a symmetric braille bar centred on a baseline, the same shape internal/ui/waveform.go draws for one of the terminal client's two Waveforms (mic or speaker).
 *
 * The terminal UI stacks a second "far" braille row above and below its "near" row on each side of centre, so one bar spans four terminal rows and eight dot-levels of amplitude. The hover's bar is only ever one braille row on each side of centre — two rows total — so this port keeps waveform.go's per-column gamma curve and centre-out fill but drops the far row: amplitude that would have spilled into it instead saturates the near row at "full".
 *
 *   silent:  ⣀⣀⣀⣀⣀⣀⣀⣀⣀⣀⣀⣀⣀⣀⣀⣀⣀⣀⣀⣀
 *            ⠉⠉⠉⠉⠉⠉⠉⠉⠉⠉⠉⠉⠉⠉⠉⠉⠉⠉⠉⠉
 */
export class Waveform {
  width: number;
  variation: number[];
  smoothed = 0;

  constructor(width: number) {
    this.width = width > 0 ? width : 40;
    this.variation = buildVariation(this.width);
  }

  /** Pushes a new amplitude sample (0-1). Rise is fast, fall is smoothed — see riseAlpha/fallAlpha above. Output: none; the eased value is read back out with render(). */
  update(amp: number): void {
    if (amp > this.smoothed) {
      this.smoothed = this.smoothed * (1 - RISE_ALPHA) + amp * RISE_ALPHA;
    } else {
      this.smoothed = this.smoothed * (1 - FALL_ALPHA) + amp * FALL_ALPHA;
    }
    if (this.smoothed < AMP_THRESHOLD) this.smoothed = 0;
  }

  /** Renders the current smoothed amplitude as two rows of braille characters, one above the centre baseline and one below it, filling outward from the centre as the level rises. Each column has its own fixed variation multiplier, so louder columns and quieter columns rise to different heights the way waveform.go's Render does. Output: [topRow, bottomRow], each this.width characters long — a silent column in each is the dim centreline character (⣀ on top, ⠉ on bottom) rather than a blank. */
  render(): string[] {
    let top = "";
    let bottom = "";
    for (let i = 0; i < this.width; i++) {
      const colAmp = this.smoothed * this.variation[i];
      if (colAmp < AMP_THRESHOLD) {
        top += String.fromCharCode(ROW0_LEVELS[1]);
        bottom += String.fromCharCode(ROW1_LEVELS[1]);
        continue;
      }
      // gamma 0.42: close to the original sensitivity, works with scaled RMS. 0.05→0.18, 0.1→0.27, 0.3→0.53, 0.7→0.82, 1.0→1.0.
      const scaled = Math.pow(colAmp, 0.42);
      let n = Math.round(scaled * 8);
      if (n < 1) n = 1;
      if (n > 8) n = 8;
      const near = Math.min(n, 4);
      top += String.fromCharCode(ROW0_LEVELS[near]);
      bottom += String.fromCharCode(ROW1_LEVELS[near]);
    }
    return [top, bottom];
  }
}

/** LevelDetail is the {"mic":0-1,"speaker":0-1} payload the daemon's "level" event carries in Detail (see internal/ipc/voice.go). */
export interface LevelDetail {
  mic: number;
  speaker: number;
}

/** Parses a "level" event's Detail string into the mic/speaker reading it carries. Input: the raw Detail JSON. Output: {mic, speaker}, each 0 when missing or not a number, and {mic: 0, speaker: 0} when detail is not valid JSON at all — a dropped or garbled level tick should read as silence, not throw. */
export function renderLevelEvent(detail: string): LevelDetail {
  try {
    const parsed = JSON.parse(detail);
    const mic = typeof parsed?.mic === "number" ? parsed.mic : 0;
    const speaker = typeof parsed?.speaker === "number" ? parsed.speaker : 0;
    return { mic, speaker };
  } catch {
    return { mic: 0, speaker: 0 };
  }
}
