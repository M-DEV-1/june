import { describe, expect, it } from "vitest";
import { LogicalSize, PhysicalPosition } from "@tauri-apps/api/dpi";
import {
  DEFAULT_HOVER_POSITION,
  dockedArea,
  edgeInset,
  fitWindow,
  hoverPlacement,
  monitorForPoint,
  placementFor,
  resolveContext,
  storedHoverPosition,
  toggleWindow,
  usableArea,
  type Desktop,
  type HoverPosition,
  type MonitorLike,
  type PlaceContext,
  type WinLike,
} from "./winplace";

/** A WinLike that records every call made on it, so a test can assert exactly which window calls a sequence made and in what order. Input: whether the window starts visible. Output: the fake, with `calls` growing as methods are invoked. */
function fakeWin(startVisible: boolean): WinLike & { calls: string[]; visible: boolean } {
  const win = {
    calls: [] as string[],
    visible: startVisible,
    async isVisible() {
      return win.visible;
    },
    async show() {
      win.visible = true;
      win.calls.push("show");
    },
    async hide() {
      win.visible = false;
      win.calls.push("hide");
    },
    async setPosition(pos: PhysicalPosition) {
      win.calls.push(`setPosition(${pos.x},${pos.y})`);
    },
    async setSize(size: LogicalSize) {
      win.calls.push(`setSize(${size.width}x${size.height})`);
    },
  };
  return win;
}

/** Builds a monitor whose work area is the monitor minus a top panel, the usual GNOME arrangement. Input: the monitor's origin and size in physical pixels, the height of the top panel, and the scale factor. Output: the monitor. */
function monitor(x: number, y: number, width: number, height: number, panel = 32, scaleFactor = 1): MonitorLike {
  return {
    position: { x, y },
    size: { width, height },
    workArea: { position: { x, y: y + panel }, size: { width, height: height - panel } },
    scaleFactor,
  };
}

// The laptop screen this was written against: 1920x1080, a 32px GNOME top bar, and an auto-hiding dash-to-dock along the bottom whose 48px icons plus padding come to 64px of clearance.
const laptop = monitor(0, 0, 1920, 1080);
const laptopWork = { x: 0, y: 32, width: 1920, height: 1048 };
const bottomDock = { edge: "bottom" as const, clearance: 64 };
const noDock = { edge: "bottom" as const, clearance: 0 };

describe("edgeInset", () => {
  it("is a twelfth of the usable height, with a floor for short screens", () => {
    expect(edgeInset(1048, 1)).toBe(87);
    expect(edgeInset(200, 1)).toBe(24);
    expect(edgeInset(2096, 2)).toBe(175);
  });
});

describe("dockedArea", () => {
  it("takes an auto-hiding dock's strip off the edge it sits on", () => {
    expect(dockedArea(laptopWork, bottomDock)).toEqual({ x: 0, y: 32, width: 1920, height: 984 });
    expect(dockedArea(laptopWork, { edge: "top", clearance: 64 })).toEqual({ x: 0, y: 96, width: 1920, height: 984 });
    expect(dockedArea(laptopWork, { edge: "left", clearance: 64 })).toEqual({ x: 64, y: 32, width: 1856, height: 1048 });
    expect(dockedArea(laptopWork, { edge: "right", clearance: 64 })).toEqual({ x: 0, y: 32, width: 1856, height: 1048 });
  });

  it("changes nothing for a dock that reserves screen space, because the work area already excludes it", () => {
    expect(dockedArea(laptopWork, noDock)).toEqual(laptopWork);
  });
});

describe("hoverPlacement", () => {
  const win = { width: 720, height: 520 };
  // What edgeInset gives for this screen once the dock's strip is off it: 984 of usable height, a twelfth of which is 82.
  const inset = edgeInset(dockedArea(laptopWork, bottomDock).height, 1);

  it("is fed the inset for the usable area, not for the whole work area", () => {
    expect(inset).toBe(82);
  });

  it("centres the window left to right at all three positions", () => {
    // 1920 wide minus the 720 window leaves 600 either side, whichever position is chosen.
    for (const position of ["top", "center", "bottom"] as HoverPosition[]) {
      expect(hoverPlacement(laptopWork, win, bottomDock, position, inset).x).toBe(600);
    }
  });

  it("hangs the top position an inset below the top of the usable area", () => {
    // The work area starts at 32, below the panel; a bottom dock takes nothing off the top; 32 + 82 of inset.
    expect(hoverPlacement(laptopWork, win, bottomDock, "top", inset)).toEqual({ x: 600, y: 114 });
  });

  it("puts the centre position halfway down the usable area", () => {
    // The dock leaves 984 of usable height starting at 32, so the window's top is 32 + (984 - 520) / 2.
    expect(hoverPlacement(laptopWork, win, bottomDock, "center", inset)).toEqual({ x: 600, y: 264 });
  });

  it("lifts the bottom position clear of both the dock and the inset, not flush against the edge", () => {
    // The usable area ends at 32 + 984 = 1016, above the 64 of dock; minus the 520 window and 82 of inset.
    expect(hoverPlacement(laptopWork, win, bottomDock, "bottom", inset)).toEqual({ x: 600, y: 414 });
    // 1080 is the bottom of the screen, so the window's own bottom edge finishes 146 pixels up from it.
    expect(1080 - (414 + 520)).toBe(146);
  });

  it("places on a monitor whose origin is not zero rather than on the first screen", () => {
    // A second 1600x900 monitor to the right of the laptop starts at x = 1920, so every coordinate carries that offset.
    const second = monitor(1920, 0, 1600, 900);
    const work = usableArea(second);
    expect(work).toEqual({ x: 1920, y: 32, width: 1600, height: 868 });
    const smaller = edgeInset(dockedArea(work, bottomDock).height, 1);
    expect(smaller).toBe(67);
    // x: 1920 + (1600 - 720) / 2. y: the usable area ends at 32 + 804 = 836, minus the 520 window and 67 of inset.
    expect(hoverPlacement(work, win, bottomDock, "bottom", smaller)).toEqual({ x: 2360, y: 249 });
    expect(hoverPlacement(work, win, bottomDock, "top", smaller)).toEqual({ x: 2360, y: 99 });
  });

  it("gives up the inset rather than push a tall hover off the bottom of the screen", () => {
    // A 950-tall window in 984 of usable height cannot take the 82 inset: 32 + 984 - 950 - 82 = -16 would put its top above the work area and its input line under the panel. The clamp keeps the top edge instead.
    expect(hoverPlacement(laptopWork, { width: 720, height: 950 }, bottomDock, "bottom", inset)).toEqual({ x: 600, y: 32 });
  });

  it("keeps a hover taller than the screen on screen at every position", () => {
    const tall = { width: 720, height: 1200 };
    for (const position of ["top", "center", "bottom"] as HoverPosition[]) {
      expect(hoverPlacement(laptopWork, tall, bottomDock, position, inset)).toEqual({ x: 600, y: 32 });
    }
  });

  it("keeps a hover wider than the screen on screen by pinning it to the left of the usable area", () => {
    expect(hoverPlacement(laptopWork, { width: 2400, height: 520 }, bottomDock, "bottom", inset)).toEqual({ x: 0, y: 414 });
  });

  it("steps around a side dock by centring in what is left of the width", () => {
    // A 64px left dock that hides itself: the window is centred in the remaining 1856, so its left edge is 64 + (1856 - 720) / 2, and the full 1048 of height is still there below it.
    expect(hoverPlacement(laptopWork, win, { edge: "left", clearance: 64 }, "bottom", inset)).toEqual({ x: 632, y: 478 });
  });
});

describe("storedHoverPosition", () => {
  /** A localStorage stand-in holding one value. Input: what getItem should answer, or the string "throw" to act like storage that is blocked. Output: the fake store. */
  const store = (value: string | null) => ({
    getItem: () => {
      if (value === "throw") throw new Error("blocked");
      return value;
    },
  });

  it("reads back each of the three positions", () => {
    expect(storedHoverPosition(store("top"))).toBe("top");
    expect(storedHoverPosition(store("center"))).toBe("center");
    expect(storedHoverPosition(store("bottom"))).toBe("bottom");
  });

  it("falls back to the default when nothing valid is stored or storage is blocked", () => {
    expect(DEFAULT_HOVER_POSITION).toBe("bottom");
    expect(storedHoverPosition(store(null))).toBe("bottom");
    expect(storedHoverPosition(store("sideways"))).toBe("bottom");
    expect(storedHoverPosition(store("throw"))).toBe("bottom");
  });
});

describe("monitorForPoint", () => {
  const second = monitor(1920, 0, 1600, 900);

  it("picks the monitor the pointer is inside", () => {
    expect(monitorForPoint([laptop, second], { x: 100, y: 100 })).toBe(laptop);
    expect(monitorForPoint([laptop, second], { x: 2500, y: 400 })).toBe(second);
  });

  it("gives the seam between two monitors to the one that starts there", () => {
    expect(monitorForPoint([laptop, second], { x: 1920, y: 400 })).toBe(second);
    expect(monitorForPoint([laptop, second], { x: 1919, y: 400 })).toBe(laptop);
  });

  it("returns null for a point on no monitor and for a pointer that could not be read", () => {
    expect(monitorForPoint([laptop, second], { x: 4000, y: 400 })).toBeNull();
    expect(monitorForPoint([laptop, second], null)).toBeNull();
  });
});

describe("placementFor", () => {
  it("scales the window, the dock's clearance and the inset by the monitor's scale factor", () => {
    // A 2x monitor, 3840x2160 physical with a 64px physical panel: the 720x520 logical window is 1440x1040 physical and the 64 of dock is 128.
    const ctx: PlaceContext = { work: { x: 0, y: 64, width: 3840, height: 2096 }, scale: 2, dock: { edge: "bottom", clearance: 64 }, position: "bottom" };
    // The usable height is 2096 - 128 = 1968, so the inset is 164. The usable area ends at 64 + 1968 = 2032, minus 1040 of window and 164 of inset.
    expect(placementFor(ctx, { width: 720, height: 520 })).toEqual({ x: 1200, y: 828 });
  });
});

/** A Desktop that answers from fixed values, so the monitor choice can be tested without a real screen. Input: the monitors, the pointer position (null if it cannot be read), and the monitor to fall back to. Output: the fake desktop. */
function fakeDesktop(monitors: MonitorLike[], pointer: { x: number; y: number } | null, focused: MonitorLike | null): Desktop {
  return {
    monitors: async () => monitors,
    pointer: async () => pointer,
    focused: async () => focused,
    dock: async () => bottomDock,
  };
}

describe("resolveContext", () => {
  const second = monitor(1920, 0, 1600, 900);
  const secondWork = { x: 1920, y: 32, width: 1600, height: 868 };

  it("uses the monitor the pointer is on and carries the chosen position through", async () => {
    const ctx = await resolveContext(fakeDesktop([laptop, second], { x: 2000, y: 500 }, laptop), "top");
    expect(ctx?.work).toEqual(secondWork);
    expect(ctx?.position).toBe("top");
  });

  it("falls back to the focused monitor when the pointer cannot be read", async () => {
    const ctx = await resolveContext(fakeDesktop([laptop, second], null, second), "bottom");
    expect(ctx?.work).toEqual(secondWork);
  });

  it("falls back to the first monitor when nothing else answers", async () => {
    const ctx = await resolveContext(fakeDesktop([laptop, second], null, null), "bottom");
    expect(ctx?.work).toEqual(laptopWork);
  });

  it("returns null when the desktop lists no monitors at all", async () => {
    expect(await resolveContext(fakeDesktop([], null, null), "bottom")).toBeNull();
  });
});

const ctx: PlaceContext = { work: laptopWork, scale: 1, dock: bottomDock, position: "bottom" };

describe("toggleWindow", () => {
  /** The options a show needs, all of them recording themselves on the window's call log. Input: the fake window and the context the open should resolve to. Output: the options and a count of focus requests. */
  function opts(win: ReturnType<typeof fakeWin>, context: PlaceContext | null) {
    const raise = { count: 0 };
    return {
      raise,
      opts: {
        beforeShow: async () => { win.calls.push("beforeShow"); },
        openContext: async () => { win.calls.push("openContext"); return context; },
        sizeToContent: async () => { win.calls.push("sizeToContent"); return { width: 720, height: 520 }; },
        raise: async () => { raise.count++; win.calls.push("raise"); },
        focusInput: () => { win.calls.push("focusInput"); },
      },
    };
  }

  it("positions a hidden window before showing it, so it never paints in the wrong place", async () => {
    const win = fakeWin(false);
    const o = opts(win, ctx);
    await toggleWindow(win, o.opts);
    expect(win.calls).toEqual([
      "beforeShow",
      "openContext",
      "sizeToContent",
      "setPosition(600,414)",
      "show",
      "raise",
      "focusInput",
    ]);
    expect(win.calls.indexOf("setPosition(600,414)")).toBeLessThan(win.calls.indexOf("show"));
    expect(o.raise.count).toBe(1);
    expect(win.calls.filter((c) => c === "setFocus")).toHaveLength(0);
  });

  it("still shows the window when no monitor could be resolved", async () => {
    const win = fakeWin(false);
    await toggleWindow(win, opts(win, null).opts);
    expect(win.calls).toEqual(["beforeShow", "openContext", "sizeToContent", "show", "raise", "focusInput"]);
  });

  it("puts a second open in the same place as the first", async () => {
    const first = fakeWin(false);
    await toggleWindow(first, opts(first, ctx).opts);
    const second = fakeWin(false);
    await toggleWindow(second, opts(second, ctx).opts);
    expect(second.calls).toEqual(first.calls);
  });

  it("hides a visible window and calls nothing else", async () => {
    const win = fakeWin(true);
    await toggleWindow(win, opts(win, ctx).opts);
    expect(win.calls).toEqual(["hide"]);
  });
});

describe("fitWindow", () => {
  it("does nothing when the height has not changed", async () => {
    const win = fakeWin(true);
    await fitWindow(win, { width: 720, height: 520 }, false, ctx);
    expect(win.calls).toEqual([]);
  });

  it("moves a growing bottom-positioned window up so its bottom edge stays put", async () => {
    const win = fakeWin(true);
    await fitWindow(win, { width: 720, height: 700 }, true, ctx);
    // The usable area still ends at 1016 and the inset is still 82, so a window 180 taller starts 180 higher: 414 - 180.
    expect(win.calls).toEqual(["setSize(720x700)", "setPosition(600,234)"]);
  });

  it("moves a growing centred window up by half of what it grew", async () => {
    const win = fakeWin(true);
    await fitWindow(win, { width: 720, height: 700 }, true, { ...ctx, position: "center" });
    // Centred at 264 when 520 tall; 180 taller moves the top up by 90.
    expect(win.calls).toEqual(["setSize(720x700)", "setPosition(600,174)"]);
  });

  it("leaves a growing top-positioned window where it is", async () => {
    const win = fakeWin(true);
    await fitWindow(win, { width: 720, height: 700 }, true, { ...ctx, position: "top" });
    expect(win.calls).toEqual(["setSize(720x700)", "setPosition(600,114)"]);
  });

  it("does nothing beyond resizing while hidden", async () => {
    const win = fakeWin(false);
    await fitWindow(win, { width: 720, height: 700 }, true, ctx);
    expect(win.calls).toEqual(["setSize(720x700)"]);
  });

  it("does nothing beyond resizing when no monitor was resolved for this open", async () => {
    const win = fakeWin(true);
    await fitWindow(win, { width: 720, height: 700 }, true, null);
    expect(win.calls).toEqual(["setSize(720x700)"]);
  });
});
