/** @vitest-environment jsdom */
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { LogicalSize, PhysicalPosition, PhysicalSize } from "@tauri-apps/api/dpi";
import type { Desktop, MonitorLike, WinLike } from "./winplace";

// jsdom does not implement matchMedia at all; main.ts reads prefers-reduced-motion at module load.
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

// The daemon itself is not what this test is about: probe resolves true so the ask goes down the live path, and ask() never resolves, so the turn stays "asking" for as long as the test needs it to.
vi.mock("./daemon", () => ({
  setPort: vi.fn(),
  setToken: vi.fn(),
  probe: vi.fn().mockResolvedValue(true),
  events: vi.fn(() => () => {}),
  context: vi.fn().mockResolvedValue(null),
  matters: vi.fn().mockResolvedValue(null),
  ask: vi.fn().mockReturnValue(new Promise(() => {})),
  // The real post, minus the base URL and the token, so a test sees each call on the stubbed fetch.
  post: vi.fn(async (path: string, body?: unknown) => {
    try {
      return await fetch(path, { method: "POST", body: body === undefined ? undefined : JSON.stringify(body) });
    } catch {
      return null;
    }
  }),
  voiceStart: vi.fn(),
  voiceStatus: vi.fn().mockResolvedValue(null),
  voiceStop: vi.fn(),
  actStart: vi.fn().mockReturnValue(new Promise(() => {})),
  actStop: vi.fn(),
  actPauseResume: vi.fn(),
  actAnswer: vi.fn(),
}));

// What the daemon puts on a task notice: the same five buttons its desktop banner offers (noticeActions in internal/proactive/notify.go).
const TASK_ACTIONS = [
  { key: "default", label: "Open in June" },
  { key: "done", label: "Done" },
  { key: "hour", label: "In an hour" },
  { key: "evening", label: "This evening" },
  { key: "tomorrow", label: "Tomorrow" },
];

// A fresh notice is a card with its answers on it: the user deals with it where it appears instead of going to the app window or to a desktop banner asking the same thing.
describe("a notice card carries its own buttons", () => {
  beforeEach(() => {
    vi.resetModules();
    document.body.innerHTML = `<div class="N" id="n" hidden></div><div class="W" id="w"></div>`;
  });

  it("uses '-' for a notice with no row of its own, the way the daemon's route expects", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(null, { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    try {
      const { dispatch } = await import("./main");
      await new Promise((r) => setTimeout(r, 0));
      dispatch({ kind: "notice", notice: { title: "Morning brief", body: "Nothing urgent.", place: "", id: "", kind: "brief", actions: TASK_ACTIONS }, hoverOpen: true });
      document.querySelector<HTMLButtonElement>('#n button.na[data-act="done"]')!.click();
      expect(fetchMock.mock.calls.some(([url]) => String(url).endsWith("/notices/brief/-/action"))).toBe(true);
    } finally {
      vi.unstubAllGlobals();
    }
  });

});

/** A WinLike and a Desktop that record what they were asked to do, so the whole Tauri path — the show, the notice-only window, the sizing and the thread cap — can be driven headless. Under jsdom getCurrentWindow() throws, so none of it used to be reachable from a test at all. Input: the window's own scale factor and the monitor's. Output: the shell to hand wireWindow, the call log, and a way to fire the toggle hotkey. */
function fakeShell(winScale = 1, monitorScale = 1) {
  const calls: string[] = [];
  let visible = false;
  let toggle: () => void = () => {};
  const win: WinLike = {
    async isVisible() {
      return visible;
    },
    async show() {
      visible = true;
      calls.push("show");
    },
    async hide() {
      visible = false;
      calls.push("hide");
    },
    async setPosition(pos: PhysicalPosition) {
      calls.push(`setPosition(${pos.x},${pos.y})`);
    },
    async setSize(size: LogicalSize | PhysicalSize) {
      calls.push(`setSize(${size.width}x${size.height})`);
    },
    async scaleFactor() {
      return winScale;
    },
  };
  // One 1920x1080 monitor with a 32px top bar and no dock, the same screen winplace.test.ts places against.
  const screen: MonitorLike = {
    position: { x: 0, y: 0 },
    size: { width: 1920, height: 1080 },
    workArea: { position: { x: 0, y: 32 }, size: { width: 1920, height: 1048 } },
    scaleFactor: monitorScale,
  };
  const desktop: Desktop = {
    monitors: async () => [screen],
    pointer: async () => ({ x: 100, y: 100 }),
    focused: async () => screen,
    dock: async () => ({ edge: "bottom", clearance: 0 }),
  };
  return {
    calls,
    shell: {
      win,
      desktop,
      raise: async () => {
        calls.push("raise");
      },
      onToggle: (run: () => void) => {
        toggle = run;
      },
    },
    pressHotkey: () => toggle(),
  };
}

describe("the window the hotkey shows", () => {
  beforeEach(async () => {
    vi.resetModules();
    vi.clearAllMocks();
    const { probe } = await import("./daemon");
    vi.mocked(probe).mockResolvedValue(true);
    document.body.innerHTML = `<div class="N" id="n" hidden></div><div class="W" id="w"></div>`;
    document.body.style.removeProperty("--thread-max");
  });

  // The show used to await the context read, which goes through AT-SPI and can block for seconds; a wedged read made the hotkey look dead.
  it("shows without waiting for the context read to answer", async () => {
    const { context } = await import("./daemon");
    vi.mocked(context).mockReturnValue(new Promise(() => {}));
    const { wireWindow } = await import("./main");
    const { shell, calls, pressHotkey } = fakeShell();
    wireWindow(shell);

    pressHotkey();
    await vi.waitFor(() => expect(calls).toContain("show"));
    expect(calls.indexOf("show")).toBeLessThan(calls.indexOf("raise"));
  });

  // The probe gives the daemon 500ms; a daemon mid-decode can miss that and still be there, and the card turning to "Not connected" sends the next question to the offline sentence instead of to the daemon.
  it("keeps the daemon up through one missed probe and gives up on the second", async () => {
    const { probe } = await import("./daemon");
    const { wireWindow } = await import("./main");
    const { shell, calls, pressHotkey } = fakeShell();
    wireWindow(shell);
    await vi.waitFor(() => expect(document.querySelector(".face")).toBeTruthy());

    vi.mocked(probe).mockResolvedValue(false);
    pressHotkey();
    await vi.waitFor(() => expect(probe).toHaveBeenCalledTimes(2));
    expect(document.querySelector(".face")).toBeTruthy();

    // Hide, then show again: the second miss in a row is the one that is believed.
    pressHotkey();
    await vi.waitFor(() => expect(calls).toContain("hide"));
    pressHotkey();
    await vi.waitFor(() => expect(probe).toHaveBeenCalledTimes(3));
    await vi.waitFor(() => expect(document.querySelector(".in")?.textContent).toContain("Not connected"));
  });

});

// The context read is what a question about what is on screen is answered from, and it is fired off behind the show rather than before it, so a question sent in the first seconds after the hotkey used to go with nothing.
describe("a question asked while the screen context is still being read", () => {
  beforeEach(async () => {
    vi.resetModules();
    vi.clearAllMocks();
    const { probe, matters } = await import("./daemon");
    vi.mocked(probe).mockResolvedValue(true);
    vi.mocked(matters).mockResolvedValue(null);
    document.body.innerHTML = `<div class="N" id="n" hidden></div><div class="W" id="w"></div>`;
  });

  it("waits for the read and sends the screen text with the question", async () => {
    const { context, ask } = await import("./daemon");
    let land: () => void = () => {};
    vi.mocked(context).mockReturnValue(
      new Promise((resolve) => {
        land = () => resolve({ app: "Brave", title: "Invoice 42", text: "Invoice 42 is due on Friday." });
      }),
    );
    vi.mocked(ask).mockReturnValue(new Promise(() => {}));

    const { dispatch } = await import("./main");
    await new Promise((r) => setTimeout(r, 0));
    dispatch({ kind: "type", value: "when is this due" });
    dispatch({ kind: "enter" });

    // Nothing has gone to the daemon yet: the read the question needs is still out.
    expect(ask).not.toHaveBeenCalled();

    land();
    await new Promise((r) => setTimeout(r, 0));
    expect(vi.mocked(ask).mock.calls[0][1]).toBe("Invoice 42 is due on Friday.");
  });
});
