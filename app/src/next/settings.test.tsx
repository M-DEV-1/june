// @vitest-environment jsdom

/** Tests for Settings: the theme control, the hotkey the daemon reports, the switches over what Ora is allowed to watch, the brains and their models, what the daemon says about this machine, and the token ledger at the foot. */

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import type { Brain, LiveModel, SettingsView, Usage, Voice } from "./api";
import { renderApp } from "./testing";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  localStorage.clear();
});

const settings: Partial<SettingsView> = {
  data_dir: "/home/you/.ora",
  store_bytes: 22020096,
  recordings_bytes: 0,
  models_bytes: 0,
  voice_model: "whisper-small",
  brain: "Claude, opus",
  embed_model: "none",
  meetings_enabled: true,
  capture_enabled: true,
  keep_audio_days: -1,
  daemon_started: new Date().toISOString(),
  version: "dev",
  hotkey: "<Control><Alt>space",
};

const brains: Brain[] = [
  { id: "claude", name: "Claude", signed_in: true, account: "max", models: ["opus", "sonnet"], model: "opus", note: "", default: true },
  { id: "grok", name: "Grok", signed_in: false, account: "", models: [], model: "", note: "not set up on this machine", default: false },
];

const usage: Usage = {
  today: { providers: [{ provider: "claude", calls: 3, input_tokens: 1200, output_tokens: 400, total_tokens: 1600 }], models: [{ provider: "claude", model: "opus", calls: 3, input_tokens: 1200, output_tokens: 400, total_tokens: 1600 }] },
  week: { providers: [{ provider: "claude", calls: 9, input_tokens: 12400, output_tokens: 3000, total_tokens: 15400 }], models: [] },
  days: [{ day: "2026-09-04", calls: 3, total_tokens: 1600 }],
  recent: [{ id: 1, when: new Date().toISOString(), provider: "claude", model: "opus", channel: "text", input_tokens: 1200, output_tokens: 400, total_tokens: 1600, duration_ms: 2400, question: "what did she say?" }],
};

describe("the first run", () => {
  it("lists the daemon's own steps while nothing can answer, and re-reads /settings when Check again is clicked", async () => {
    const steps = ["Set GEMINI_API_KEY in ~/.config/ora/env.", "Or sign in with the Claude CLI: run claude login."];
    const { calls } = renderApp({ settings: { ...settings, first_run: { gemini_key: false, codex_login: false, claude_cli: false, local_model: false, steps } } }, { place: "settings" });
    expect(await screen.findByText("Ora cannot answer yet")).toBeDefined();
    // Each step reads verbatim as the daemon's own sentence — the tokens in it a person would actually type are just marked as code inside it.
    const drawn = Array.from(document.querySelectorAll("li")).map((li) => li.textContent);
    for (const step of steps) expect(drawn).toContain(step);
    const before = calls.filter((c) => c.path === "/settings").length;
    await userEvent.click(screen.getByRole("button", { name: "Check again" }));
    await waitFor(() => expect(calls.filter((c) => c.path === "/settings").length).toBeGreaterThan(before));
  });

  it("draws nothing once the daemon has no steps left, and nothing at all for a daemon too old to send the field", async () => {
    renderApp({ settings: { ...settings, first_run: { gemini_key: true, codex_login: false, claude_cli: false, local_model: false, steps: [] } } }, { place: "settings" });
    await screen.findByText("This machine");
    expect(screen.queryByText("Ora cannot answer yet")).toBeNull();
    cleanup();
    renderApp({ settings }, { place: "settings" });
    await screen.findByText("This machine");
    expect(screen.queryByText("Ora cannot answer yet")).toBeNull();
  });
});

describe("Settings", () => {
  it("keeps the theme choice in the store when another segment is picked", async () => {
    const { store } = renderApp({ settings }, { place: "settings" });
    await userEvent.click(await screen.findByRole("tab", { name: "Dark" }));
    expect(store.getState().settings.theme).toBe("dark");
  });

  it("opens on the position stored under the key the hover reads, and writes back there when another is picked", async () => {
    localStorage.setItem("ora-hover-position", "top");
    renderApp({ settings }, { place: "settings" });
    expect(await screen.findByRole("tab", { name: "Top", selected: true })).toBeDefined();
    await userEvent.click(screen.getByRole("tab", { name: "Center" }));
    expect(localStorage.getItem("ora-hover-position")).toBe("center");
  });

  it("draws the accelerator the daemon reports, key by key", async () => {
    renderApp({ settings }, { place: "settings" });
    expect(await screen.findByText("Ctrl")).toBeDefined();
    expect(screen.getByText("Alt")).toBeDefined();
    expect(screen.getByText("Space")).toBeDefined();
  });

  it("falls back to the keys the installer registers when the daemon reports none", async () => {
    renderApp({ settings: { ...settings, hotkey: "" } }, { place: "settings" });
    expect(await screen.findByText("Ctrl")).toBeDefined();
    expect(screen.getByText("Space")).toBeDefined();
  });

  it("pauses and resumes what Ora is allowed to watch", async () => {
    const { calls } = renderApp({ settings }, { place: "settings" });
    const watching = await screen.findByLabelText("Watching the screen");
    await waitFor(() => expect(watching.getAttribute("aria-checked")).toBe("true"));
    await userEvent.click(watching);
    await waitFor(() => expect(calls.some((c) => c.method === "POST" && c.path === "/pause")).toBe(true));
  });

  // A switch that cannot move still reads as a switch someone could work if they tried. The config file decides this one, so the row states it as a fact.
  it("shows whether meetings are being recorded but does not offer to change it", async () => {
    renderApp({ settings }, { place: "settings" });
    expect(await screen.findByText("Recording meetings")).toBeDefined();
    // The row reads "Off" until /settings answers, so this waits for the daemon's own value.
    expect(await screen.findByText("On")).toBeDefined();
    expect(screen.queryByLabelText("Recording meetings")).toBeNull();
  });

  it("says which brains are signed in and writes the model that is picked", async () => {
    const { calls } = renderApp({ settings, brains }, { place: "settings" });
    expect(await screen.findByText("signed in · max")).toBeDefined();
    expect(screen.getByText("not signed in")).toBeDefined();
    expect(screen.getByText("not set up on this machine")).toBeDefined();
    await userEvent.click(screen.getByRole("button", { name: "sonnet" }));
    await waitFor(() => expect(calls.find((c) => c.method === "POST" && c.path === "/brains")?.body).toEqual({ brain: "claude", model: "sonnet" }));
  });

  it("shows the Claude usage toggle on by default and posts turning it off", async () => {
    const { calls } = renderApp({ settings, brains }, { place: "settings" });
    const toggle = await screen.findByLabelText("Show Claude plan usage");
    await waitFor(() => expect(toggle.getAttribute("aria-checked")).toBe("true"));
    await userEvent.click(toggle);
    await waitFor(() => expect(calls.find((c) => c.method === "POST" && c.path === "/settings")?.body).toEqual({ claude_usage_from_login: false }));
    await waitFor(() => expect(toggle.getAttribute("aria-checked")).toBe("false"));
  });

  it("says what the daemon is running and how much it has written down", async () => {
    renderApp({ settings }, { place: "settings" });
    expect(await screen.findByText("This machine")).toBeDefined();
    expect(screen.getByText("/home/you/.ora")).toBeDefined();
    expect(screen.getByText(/21\.0 MB of memory/)).toBeDefined();
    expect(screen.getByText("nothing — search is words only")).toBeDefined();
    expect(screen.getByText("as long as you leave it there")).toBeDefined();
  });

});

const voices: Voice[] = [
  { name: "Iapetus", trait: "Clear", current: true },
  { name: "Puck", trait: "Upbeat", current: false },
];

const models: LiveModel[] = [
  { name: "gemini-3.1-flash-live-preview", label: "Gemini 3.1 Flash Live", trait: "Fast — about two seconds to first word, one tone, hears everything", current: true },
  { name: "gemini-2.5-flash-native-audio-preview-12-2025", label: "Gemini 2.5 Native Audio", trait: "Warm — five to eight seconds, but it has moods and can ignore the room", current: false },
];

describe("the voice model picker", () => {
  it("shows both Live models with the current one marked, and posts the other one's name when picked", async () => {
    const { calls } = renderApp({ settings, voices, models }, { place: "settings" });
    const trigger = await screen.findByRole("button", { name: "Model" });
    expect(within(trigger).getByText("Gemini 3.1 Flash Live")).toBeDefined();
    await userEvent.click(trigger);
    const menu = within(await screen.findByRole("menu"));
    expect(menu.getByText("Gemini 3.1 Flash Live")).toBeDefined();
    expect(menu.getByText("Gemini 2.5 Native Audio")).toBeDefined();
    await userEvent.click(menu.getByRole("menuitem", { name: /Gemini 2.5 Native Audio/ }));
    await waitFor(() => expect(calls.find((c) => c.method === "POST" && c.path === "/voices")?.body).toEqual({ model: "gemini-2.5-flash-native-audio-preview-12-2025" }));
  });

  it("says it could not change the voice model when the daemon refuses", async () => {
    renderApp({ settings, voices, models, fails: ["POST /voices"] }, { place: "settings" });
    await userEvent.click(await screen.findByRole("button", { name: "Model" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: /Gemini 2.5 Native Audio/ }));
    expect(await screen.findByText("Could not change the voice model")).toBeDefined();
  });
});

describe("the token ledger", () => {
  it("opens on three figures and the week's bars before any table", async () => {
    renderApp({ settings, usage }, { place: "settings" });
    expect(await screen.findByText("Token use")).toBeDefined();
    // 1,600 spent today over 3 calls, and 15,400 over the week, said as figures a person reads at a glance. Today's total reads "1.6k" twice: once as the figure and once as the label on that day's bar.
    expect(await screen.findAllByText("1.6k")).toHaveLength(2);
    expect(screen.getByText("calls today")).toBeDefined();
    expect(screen.getByText("15.4k")).toBeDefined();
    expect(screen.getByTitle("2026-09-04: 1,600 tokens over 3 calls")).toBeDefined();
  });

  it("sits at the foot of Settings with what was spent today and over the week", async () => {
    renderApp({ settings, usage }, { place: "settings" });
    expect(await screen.findByText("Today")).toBeDefined();
    expect(screen.getByText("Last seven days")).toBeDefined();
    // 1,600 is today's total for the provider and again for its one model, which is the row indented under it.
    expect(screen.getAllByText("1,600")).toHaveLength(3);
    expect(screen.getByText("15,400")).toBeDefined();
    expect(screen.getByText("what did she say?")).toBeDefined();
    expect(screen.getByText("2.4s")).toBeDefined();
  });

  it("says nothing has been spent rather than drawing four empty tables", async () => {
    renderApp({ settings }, { place: "settings" });
    expect(await screen.findByText("Nothing asked yet")).toBeDefined();
    expect(screen.getByText(/No tokens have been spent on this machine/)).toBeDefined();
  });

  it("says what one question costs, in tokens, because the daemon reports no prices", async () => {
    renderApp({ settings, usage }, { place: "settings" });
    // 15,400 tokens over 9 calls this week is 1,711 a question, which reads as 1.7k beside the three totals.
    expect(await screen.findByText("a question, this week")).toBeDefined();
    expect(screen.getByText("1.7k")).toBeDefined();
  });

  it("says how much of the input came out of the provider's cache, once the daemon reports it", async () => {
    const cached = { ...usage.recent[0], input_tokens: 1200, cached_input_tokens: 900 };
    renderApp({ settings, usage: { ...usage, recent: [cached] } }, { place: "settings" });
    expect(await screen.findByText(/900 came back out of the provider's cache/)).toBeDefined();
    expect(screen.getByText(/300 were read afresh/)).toBeDefined();
  });

  it("says nothing about caching while the daemon sends no cached figure", async () => {
    renderApp({ settings, usage }, { place: "settings" });
    await screen.findByText("Recent calls");
    expect(screen.queryByText(/out of the provider's cache/)).toBeNull();
  });

  it("warns once more than four fifths of the plan's allowance has gone, and not before", async () => {
    renderApp({ settings, usage: { ...usage, budget_used_fraction: 0.91 } }, { place: "settings" });
    expect(await screen.findByText("91% of this plan's allowance has gone.")).toBeDefined();
    cleanup();
    renderApp({ settings, usage: { ...usage, budget_used_fraction: 0.5 } }, { place: "settings" });
    await screen.findByText("Recent calls");
    expect(screen.queryByText(/allowance has gone/)).toBeNull();
  });

  it("bars each recent call to its share of the priciest one in the list, so an expensive question stands out without reading every row", async () => {
    // A row's own total is already the whole question's cost: recordTokenUse in internal/ipc/ipc.go files one row per finished ask, its counts summed over every round of that ask's tool loop (see agent.TurnTrace.Usage) — so 60,000 here is one question's full round-trip cost, not one round of it.
    const heavy = { id: 20, when: new Date().toISOString(), provider: "codex", model: "gpt-5.5", channel: "text", input_tokens: 55000, output_tokens: 5000, total_tokens: 60000, duration_ms: 90000, question: "rebuild the whole page" };
    const light = { id: 21, when: new Date().toISOString(), provider: "codex", model: "gpt-5.5", channel: "text", input_tokens: 500, output_tokens: 100, total_tokens: 600, duration_ms: 2000, question: "what time is it" };
    renderApp({ settings, usage: { ...usage, recent: [heavy, light] } }, { place: "settings" });
    await screen.findByText("rebuild the whole page");
    expect((screen.getByTestId("cost-bar-20") as HTMLElement).style.backgroundImage).toContain("100%");
    // 600 / 60,000 rounds to 1%, not 0 — a cheap question still shows a sliver rather than reading as free.
    expect((screen.getByTestId("cost-bar-21") as HTMLElement).style.backgroundImage).toContain("1%");
  });

  it("is the last section of Settings and not a destination of its own", async () => {
    renderApp({ settings, usage }, { place: "settings" });
    await screen.findByText("This machine");
    expect(within(await screen.findByRole("main")).getByText("Recent calls")).toBeDefined();
    expect(screen.queryByRole("button", { name: "Usage" })).toBeNull();
  });
});
