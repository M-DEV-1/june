/** @vitest-environment jsdom */
/** Tests for the ?mock=1&voice=1 fake live-voice session: fakeLevelAt's envelope is pure and checked directly, and startMockVoice is checked end to end against the real main.ts (with only the daemon stubbed out, same as main.test.ts) since the whole point is that it drives the real composer waveform with no daemon and no microphone. */
import { beforeEach, describe, expect, it, vi } from "vitest";

// jsdom does not implement matchMedia; main.ts reads prefers-reduced-motion at module load (see main.test.ts, which needs the same stub).
if (!window.matchMedia) {
  window.matchMedia = ((query: string) =>
    ({
      matches: false,
      media: query,
      onchange: null,
      addListener: () => {},
      removeListener: () => {},
      addEventListener: () => {},
      removeEventListener: () => {},
      dispatchEvent: () => false,
    }) as unknown as MediaQueryList) as typeof window.matchMedia;
}

// mock.ts calls into main.ts's dispatch, which calls the daemon module; stub it out exactly as main.test.ts does so connect() settles instead of hitting a real network call.
vi.mock("./daemon", () => ({
  setPort: vi.fn(),
  setToken: vi.fn(),
  probe: vi.fn().mockResolvedValue(false),
  events: vi.fn(() => () => {}),
  context: vi.fn().mockResolvedValue(null),
  matters: vi.fn().mockResolvedValue(null),
  ask: vi.fn().mockReturnValue(new Promise(() => {})),
  endpoint: vi.fn().mockReturnValue({ base: "", token: undefined }),
  voiceStart: vi.fn(),
  voiceStatus: vi.fn().mockResolvedValue(null),
  voiceStop: vi.fn(),
  actStart: vi.fn().mockReturnValue(new Promise(() => {})),
  actStop: vi.fn(),
  actPauseResume: vi.fn(),
  actAnswer: vi.fn(),
}));

describe("fakeLevelAt", () => {
  it("bursts the mic between 0.3 and 0.9 for the first second of each 1.5s cycle", async () => {
    const { fakeLevelAt } = await import("./mock");
    for (let ms = 0; ms < 1000; ms += 50) {
      const { mic } = fakeLevelAt(ms);
      expect(mic).toBeGreaterThanOrEqual(0.3);
      expect(mic).toBeLessThanOrEqual(0.9);
    }
  });

  it("drops the mic to near-silence for the half second after each burst", async () => {
    const { fakeLevelAt } = await import("./mock");
    for (let ms = 1000; ms < 1500; ms += 50) {
      expect(fakeLevelAt(ms).mic).toBeLessThan(0.1);
    }
  });

  it("keeps the speaker silent through the first (even) burst", async () => {
    const { fakeLevelAt } = await import("./mock");
    for (let ms = 0; ms < 1500; ms += 50) {
      expect(fakeLevelAt(ms).speaker).toBe(0);
    }
  });

  it("gives the speaker its own 0.3-0.9 burst on the second (odd) cycle, silent in the gap after", async () => {
    const { fakeLevelAt } = await import("./mock");
    let sawLoud = false;
    for (let ms = 1500; ms < 2500; ms += 50) {
      const { speaker } = fakeLevelAt(ms);
      expect(speaker).toBeGreaterThanOrEqual(0.3);
      expect(speaker).toBeLessThanOrEqual(0.9);
      sawLoud = true;
    }
    expect(sawLoud).toBe(true);
    for (let ms = 2500; ms < 3000; ms += 50) {
      expect(fakeLevelAt(ms).speaker).toBe(0);
    }
  });

  it("is deterministic, not drawn from Math.random", async () => {
    const { fakeLevelAt } = await import("./mock");
    expect(fakeLevelAt(1234)).toEqual(fakeLevelAt(1234));
    expect(fakeLevelAt(2222)).toEqual(fakeLevelAt(2222));
  });
});

describe("startMockVoice", () => {
  beforeEach(() => {
    vi.resetModules();
    vi.clearAllMocks();
    document.body.innerHTML = `<div class="N" id="n" hidden></div><div class="W" id="w"></div>`;
  });

  it("does nothing without both ?mock and ?voice=1", async () => {
    const { startMockVoice } = await import("./mock");
    startMockVoice(new URLSearchParams(""));
    startMockVoice(new URLSearchParams("mock=1"));
    startMockVoice(new URLSearchParams("voice=1"));
    await new Promise((r) => setTimeout(r, 60));
    // None of those matched, so main.ts (and its "#w" render) was never even loaded.
    expect(document.querySelector(".vwave")).toBeNull();
  });

  it("starts a fake session and animates the mic row off real 50ms ticks", async () => {
    const { startMockVoice } = await import("./mock");

    startMockVoice(new URLSearchParams("mock=1&voice=1"));
    // startMockVoice's dynamic import("./main"), main.ts's own connect() (probe/context/matters/voiceStatus, all stubbed above) and a handful of real 50ms ticks all need actual turns of the event loop; the repaint itself is then coalesced to one per animation frame (see scheduleVoiceWaveRepaint in main.ts).
    await new Promise((r) => setTimeout(r, 250));
    await new Promise((r) => requestAnimationFrame(r));

    const micRows = () =>
      Array.from(document.querySelectorAll(".vw-mic .vw-row")).map(
        (el) => el.textContent ?? "",
      );
    const [top] = micRows();
    // A few ticks of the 0.3-0.9 burst have landed by now, so the row is no longer the flat silent baseline.
    expect(top).toBeTruthy();
    expect(top).not.toBe("⣀".repeat(top.length));
  });
});
