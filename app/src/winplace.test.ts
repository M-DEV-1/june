import { describe, expect, it } from "vitest";
import { LogicalSize, PhysicalPosition, PhysicalSize } from "@tauri-apps/api/dpi";
import {
  dockedArea,
  edgeInset,
  fitWindow,
  hoverPlacement,
  noticePlacement,
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

/** A WinLike that records every call made on it, so a test can assert exactly which window calls a sequence made and in what order. Input: whether the window starts visible. Output: the fake, with `calls` growing as methods are invoked and `sizes` holding the size objects setSize was handed, so a test can tell a logical size from a physical one. */
function fakeWin(startVisible: boolean): WinLike & { calls: string[]; sizes: (LogicalSize | PhysicalSize)[]; visible: boolean } {
  const win = {
    calls: [] as string[],
    sizes: [] as (LogicalSize | PhysicalSize)[],
    visible: startVisible,
    async scaleFactor() {
      return 1;
    },
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
    async setSize(size: LogicalSize | PhysicalSize) {
      win.sizes.push(size);
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

describe("hoverPlacement", () => {
  const win = { width: 720, height: 520 };
  // What edgeInset gives for this screen once the dock's strip is off it: 984 of usable height, a twelfth of which is 82.
  const inset = edgeInset(dockedArea(laptopWork, bottomDock).height, 1);

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

  it("falls back to the default when nothing valid is stored or storage is blocked", () => {
    expect(storedHoverPosition(store(null))).toBe("center");
    expect(storedHoverPosition(store("sideways"))).toBe("center");
    expect(storedHoverPosition(store("throw"))).toBe("center");
  });
});

describe("monitorForPoint", () => {
  const second = monitor(1920, 0, 1600, 900);

  it("gives the seam between two monitors to the one that starts there", () => {
    expect(monitorForPoint([laptop, second], { x: 1920, y: 400 })).toBe(second);
    expect(monitorForPoint([laptop, second], { x: 1919, y: 400 })).toBe(laptop);
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

  // A throw anywhere in the placement sequence used to leave the hotkey looking dead: the window stayed hidden and the rejection surfaced only in the log.
  it("still shows the window when setPosition throws", async () => {
    const win = fakeWin(false);
    const o = opts(win, ctx);
    win.setPosition = async () => {
      throw new Error("the compositor refused");
    };
    await toggleWindow(win, o.opts);
    expect(win.calls).toEqual(["beforeShow", "openContext", "sizeToContent", "show", "raise", "focusInput"]);
    expect(win.visible).toBe(true);
  });

  it("still shows the window when the daemon read before it throws", async () => {
    const win = fakeWin(false);
    const o = opts(win, ctx);
    o.opts.beforeShow = async () => {
      throw new Error("no daemon");
    };
    await toggleWindow(win, o.opts);
    expect(win.calls).toEqual(["show", "raise", "focusInput"]);
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
    expect(win.sizes[0].type).toBe("Logical");
  });

  // A LogicalSize is converted by Tauri with the window's own scale factor while the position is worked out with the pointer monitor's, so on a 1x + 2x desk the window was sized against one screen and placed against the other. Both numbers now come from the one scale factor in the context.
  it("sizes in physical pixels with the same monitor scale the position uses", async () => {
    const oneX = fakeWin(true);
    await fitWindow(oneX, { width: 720, height: 520 }, true, ctx);
    expect(oneX.sizes[0].type).toBe("Physical");
    expect([oneX.sizes[0].width, oneX.sizes[0].height]).toEqual([720, 520]);

    const twoX = fakeWin(true);
    const hidpi: PlaceContext = { work: { x: 0, y: 64, width: 3840, height: 2096 }, scale: 2, dock: bottomDock, position: "bottom" };
    await fitWindow(twoX, { width: 720, height: 520 }, true, hidpi);
    expect([twoX.sizes[0].width, twoX.sizes[0].height]).toEqual([1440, 1040]);
    // The physical size the window is given and the physical position it is moved to are now both in the 2x monitor's own pixels.
    expect(twoX.calls[1]).toBe("setPosition(1200,828)");
  });

  it("leaves the moving to its caller when asked only to resize, which is what a notice-only window needs", async () => {
    const win = fakeWin(true);
    await fitWindow(win, { width: 456, height: 160 }, true, ctx, false);
    expect(win.calls).toEqual(["setSize(456x160)"]);
  });
});

describe("noticePlacement", () => {
  /** A placement context on one monitor, for the notice placement to read. Input: the work area, the scale and the dock. Output: the context. */
  const ctx = (work: typeof laptopWork, scale: number, dock: { edge: "bottom" | "left" | "right" | "top"; clearance: number }): PlaceContext =>
    ({ work, scale, dock, position: "bottom" });

  it("puts a notice-only window under the top bar at the right of the usable area, beside the tray", () => {
    // 1920 wide minus the 456 window is 1464; the bar ends at y=32 and the window starts 8 physical pixels under it.
    expect(noticePlacement(ctx(laptopWork, 1, noDock), { width: 456, height: 160 })).toEqual({ x: 1464, y: 40 });
  });

  it("scales the window and the gap under the bar with the monitor", () => {
    // At scale 2 the 456 logical window is 912 physical, so it starts 912 back from the right edge of a 3840-wide area.
    expect(noticePlacement(ctx({ x: 0, y: 64, width: 3840, height: 2096 }, 2, noDock), { width: 456, height: 160 })).toEqual({ x: 2928, y: 80 });
  });

  it("keeps clear of an auto-hiding dock on the right edge", () => {
    // A 64 logical-pixel dock on the right reserves no screen space, so the placement has to take it off the work area itself: 1920 - 64 - 456 = 1400.
    expect(noticePlacement(ctx(laptopWork, 1, { edge: "right", clearance: 64 }), { width: 456, height: 160 })).toEqual({ x: 1400, y: 40 });
  });

  it("starts a window too big for the screen on the screen rather than off its left or bottom edge", () => {
    // 720 logical at scale 2 is 1440 physical, wider than this 1280-wide area, and 900 physical is taller than it.
    expect(noticePlacement(ctx({ x: 100, y: 32, width: 1280, height: 700 }, 2, noDock), { width: 720, height: 450 })).toEqual({ x: 100, y: 32 });
  });
});
