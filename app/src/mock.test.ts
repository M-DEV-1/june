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

  it(
    "shows Ora's silent row while only the mic bursts, and animates it once the speaker's own (odd-cycle) burst lands",
    async () => {
      const { startMockVoice } = await import("./mock");

      startMockVoice(new URLSearchParams("mock=1&voice=1"));
      // Index 1 is near-top (see waveform.ts's render(): far-top, near-top, near-bottom, far-bottom).
      const nearTop = () =>
        Array.from(document.querySelectorAll(".vw-spk .vw-row"))[1]
          ?.textContent ?? "";
      // Polls real wall-clock time rather than assuming a fixed number of the fake session's 50ms ticks have landed by some fixed wait: under a loaded test run, real setInterval ticks can lag behind wall-clock time, so a single fixed sleep is flaky. This instead waits, a real animation frame at a time, for whatever real time it actually takes.
      const waitUntil = async (
        cond: () => boolean,
        budgetMs: number,
      ): Promise<void> => {
        const start = Date.now();
        while (!cond()) {
          if (Date.now() - start > budgetMs) return;
          await new Promise((r) => setTimeout(r, 20));
          await new Promise((r) => requestAnimationFrame(r));
        }
      };

      // Ora's row exists and sits at its silent baseline the moment the session starts (this is the first, even cycle: the mic bursts, but there is no mic row left to show it on).
      await waitUntil(() => nearTop() !== "", 2000);
      const silentNearTop = nearTop();
      expect(silentNearTop).toBeTruthy();
      expect(silentNearTop).toBe("⣀".repeat(silentNearTop.length));
      expect(document.querySelector(".vw-mic")).toBeNull();

      // The second (odd) cycle bursts the speaker for its first second (see fakeLevelAt) — wait for that to actually show up in the row rather than assuming a fixed amount of wall-clock time reached it.
      await waitUntil(() => nearTop() !== silentNearTop, 8000);
      expect(nearTop()).not.toBe(silentNearTop);
    },
    10000,
  );
});
