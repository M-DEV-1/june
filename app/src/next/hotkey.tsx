/** The shortcut that opens June: the one in effect, a plain line when it does not work, and the editor that records a new one, asks the daemon whether it is free the moment it is pressed, and saves it only when it is. Settings and the last setup screen both draw it from here, so they say the same thing on Windows and on Linux. The shortcut shown is always the one GET /settings reports; the only chord written here is the one default, for the button that goes back to it. */

import { useEffect, useRef, useState, type KeyboardEvent as ReactKeyboardEvent, type ReactNode } from "react";
import { skipToken } from "@reduxjs/toolkit/query/react";
import { Loader2 } from "lucide-react";

import { Button } from "@/components/ui/button";
import { errorMessage, errorStatus, errorWord, useHotkeyCheckQuery, useSaveSettingsMutation, useSettingsQuery, type HotkeyCheck, type HotkeyStatus, type SettingsView } from "./api";
import { hotkeyKeys } from "./format";
import { ON_WINDOWS } from "./parts";
import { ui, useAppDispatch } from "./store";

/** June's one default shortcut, which POST /settings goes back to when sent "". */
const DEFAULT_SHORTCUT = "Ctrl+Alt+Space";

/** The keys that only change another key, which the editor shows while they are held and never records on their own. */
const MODIFIER_KEYS = new Set(["Control", "Alt", "AltGraph", "Shift", "Meta", "OS", "Super", "Hyper"]);

/** The keys June knows by name besides letters, digits and F1 to F24, by the code a key press reports, spelled the way the shortcut is saved — the names the daemon's ParseHotkey and the window's shortcut plugin both read. */
const NAMED_KEYS: Record<string, string> = {
  Space: "Space",
  Enter: "Enter",
  NumpadEnter: "Enter",
  Tab: "Tab",
  Backspace: "Backspace",
  Delete: "Delete",
  Insert: "Insert",
  Home: "Home",
  End: "End",
  PageUp: "PageUp",
  PageDown: "PageDown",
  ArrowUp: "Up",
  ArrowDown: "Down",
  ArrowLeft: "Left",
  ArrowRight: "Right",
  Comma: "Comma",
  Period: "Period",
  Minus: "Minus",
  Equal: "Equal",
  Semicolon: "Semicolon",
  Slash: "Slash",
  Backslash: "Backslash",
  Quote: "Quote",
  Backquote: "Backquote",
  BracketLeft: "BracketLeft",
  BracketRight: "BracketRight",
};

/** The parts of a key press the editor reads. */
type Pressed = { key: string; code: string; ctrlKey: boolean; altKey: boolean; shiftKey: boolean; metaKey: boolean };

/** The modifiers held during a key press, in the order a shortcut is spelled. Input: the press. Output: some of "Ctrl", "Alt", "Shift", "Super". */
function modifiersOf(e: Pressed): string[] {
  const out: string[] = [];
  if (e.ctrlKey) out.push("Ctrl");
  if (e.altKey) out.push("Alt");
  if (e.shiftKey) out.push("Shift");
  if (e.metaKey) out.push("Super");
  return out;
}

/** The key a press names, spelled the way a shortcut is saved. Input: the press. Output: "J", "7", "F5", "Space" and the like, or undefined for a key June cannot use. A letter or digit is read off the character the keyboard layout gives, because that is the key Windows and GNOME bind; the physical key is the fallback for when a modifier turned the character into something else. */
function keyName(e: Pressed): string | undefined {
  if (/^[a-z0-9]$/i.test(e.key) && !e.code.startsWith("Numpad")) return e.key.toUpperCase();
  const letter = /^Key([A-Z])$/.exec(e.code);
  if (letter) return letter[1];
  const digit = /^Digit([0-9])$/.exec(e.code);
  if (digit) return digit[1];
  if (/^F([1-9]|1[0-9]|2[0-4])$/.test(e.code)) return e.code;
  return NAMED_KEYS[e.code];
}

/** Why a combination cannot be a shortcut, before the daemon is asked. Input: its modifiers and its key. Output: the sentence, or "" when it may be. Any combination with Ctrl, Alt or the Windows key may be, even one other apps use inside their windows — the user asked to set any key they want, and the daemon's check warns about those — so only a shortcut another program already holds is refused, by the check. Shift alone is not enough with a letter, a digit or Space, because taking Shift+A would take capital A away from every app; F13 to F24 are on no ordinary keyboard and may stand alone. */
function ruleProblem(mods: string[], key: string): string {
  const fn = /^F(\d+)$/.exec(key);
  const n = fn ? Number(fn[1]) : 0;
  if (n < 13 && !mods.some((m) => m !== "Shift") && !(mods.length > 0 && n > 0)) return "Not a valid shortcut. Add Ctrl or Alt.";
  return "";
}

/** What a key is called on a keycap: the Windows key is "Win" on Windows, though it is saved as Super. */
function capLabel(key: string): string {
  return key === "Super" && ON_WINDOWS ? "Win" : key;
}

/** The keys of a shortcut, drawn as keycaps. Input: the keys, and whether to draw them in the muted colour. Output: the row. */
export function Keys({ keys, muted }: { keys: string[]; muted?: boolean }) {
  return (
    <span className="inline-flex flex-wrap items-center gap-1">
      {keys.map((k) => (
        <kbd key={k} className={`rounded-xs border border-hairline-strong px-1.5 py-0.5 text-micro uppercase ${muted ? "text-muted-foreground" : "text-foreground"}`}>
          {capLabel(k)}
        </kbd>
      ))}
    </span>
  );
}

/** What the window knows about the shortcut. */
type Shortcut = {
  /** The keys in effect, empty when there are none. */
  keys: string[];
  status: HotkeyStatus;
  /** The line to show under it. */
  hint: string;
  /** Whether the user can pick one here. Not on a desktop June cannot set a shortcut on, nor before GET /settings has answered. */
  canChange: boolean;
};

/** Reads the shortcut off GET /settings. Input: what it answered, or undefined before it has. Output: the shortcut. A daemon too old to say whether its shortcut works is taken at its word that the keys it names do. */
function shortcutOf(s?: SettingsView): Shortcut {
  const keys = hotkeyKeys(s?.hotkey ?? "");
  const status: HotkeyStatus = s?.hotkey_status ?? (keys.length ? "ok" : "unknown");
  const note = (s?.hotkey_note ?? "").trim();
  let hint: string;
  if (status === "taken") hint = /choose|pick/i.test(note) ? note : `${note || "Your shortcut is used by another app."} Choose your own shortcut.`;
  else if (status === "pending") hint = note || "Setting up your shortcut…";
  else if (status === "unsupported") hint = note || "June can't set a shortcut on this computer.";
  else if (!keys.length) hint = note || "No shortcut yet.";
  else hint = note || "Opens June from anywhere.";
  return { keys: status === "taken" ? [] : keys, status, hint, canChange: Boolean(s) && status !== "unsupported" };
}

/** The line under the editor about the combination pressed. Input: the daemon's answer. Output: the words, and whether they mean the shortcut can be saved. */
function checkLine(c: HotkeyCheck): { text: string; good: boolean } {
  const good = c.available || c.reason === "current";
  if (c.reason === "current") return { text: "This is your shortcut now.", good };
  if (good) return { text: "Available.", good };
  // A check that could not run is also answered "taken", with a note that says so rather than blaming another app.
  if (c.reason === "taken") return { text: c.note && !/another app/i.test(c.note) ? c.note : "Used by another app.", good };
  if (c.reason === "unsupported") return { text: c.note || "June can't set a shortcut on this computer.", good };
  // A combination other apps use is a fine shortcut in itself, so its note is not put after "Not a valid shortcut".
  if (/other apps/i.test(c.note)) return { text: c.note, good };
  return { text: c.note ? `Not a valid shortcut. ${c.note}` : "Not a valid shortcut.", good };
}

/** The editor: a box that records the next key combination pressed in it, the daemon's word on whether it is free, and Save, which is only on when it is. Escape gives up. Input: the shortcut in effect, what to do once it is saved or given up, and any classes for the outer block. Output: the editor. */
export function ShortcutEditor({ current, onDone, className }: { current: string; onDone: () => void; className?: string }) {
  const dispatch = useAppDispatch();
  const box = useRef<HTMLDivElement>(null);
  // The modifiers held right now, shown while the rest of the combination is still to come.
  const [held, setHeld] = useState<string[]>([]);
  // The last combination pressed, as keycaps, whether or not it can be used.
  const [shown, setShown] = useState<string[]>([]);
  // The combination to ask about and save; reset is the default, which is saved as "".
  const [picked, setPicked] = useState<{ accel: string; reset: boolean } | undefined>(undefined);
  const [problem, setProblem] = useState("");
  const [failed, setFailed] = useState("");
  const [save, { isLoading: saving }] = useSaveSettingsMutation();
  const check = useHotkeyCheckQuery(picked ? picked.accel : skipToken, { refetchOnMountOrArgChange: true });
  const answer = picked ? check.currentData : undefined;
  const verdict = answer ? checkLine(answer) : undefined;
  const isDefault = hotkeyKeys(current).join("+") === DEFAULT_SHORTCUT;

  useEffect(() => {
    box.current?.focus();
  }, []);

  /** Asks about one combination. */
  const pick = (accel: string, reset: boolean) => {
    setProblem("");
    setFailed("");
    setShown(accel.split("+"));
    setPicked({ accel, reset });
  };

  const onKeyDown = (e: ReactKeyboardEvent<HTMLDivElement>) => {
    // Tab and Shift+Tab move on to Save and Cancel as they do everywhere else, or the editor could not be used without a mouse; neither is a shortcut on its own anyway.
    if (e.key === "Tab" && !e.ctrlKey && !e.altKey && !e.metaKey) {
      setHeld([]);
      return;
    }
    // Every other key pressed here is the shortcut being recorded, so none of them reaches the window's own keys (Ctrl+K, the arrows) or types anywhere.
    e.preventDefault();
    e.stopPropagation();
    if (e.repeat) return;
    if (e.key === "Escape") {
      onDone();
      return;
    }
    const mods = modifiersOf(e);
    if (MODIFIER_KEYS.has(e.key)) {
      setHeld(mods);
      return;
    }
    setHeld([]);
    const key = keyName(e);
    if (!key) {
      setPicked(undefined);
      setShown([]);
      setFailed("");
      setProblem("June can't use that key. Try a letter, a number, Space or an F key.");
      return;
    }
    const rule = ruleProblem(mods, key);
    if (rule) {
      setPicked(undefined);
      setShown([...mods, key]);
      setFailed("");
      setProblem(rule);
      return;
    }
    pick([...mods, key].join("+"), false);
  };

  const commit = async () => {
    if (!picked || !answer || !verdict?.good) return;
    setFailed("");
    try {
      await save({ hotkey: picked.reset ? "" : answer.hotkey || picked.accel }).unwrap();
      dispatch(ui.noticed({ text: "Shortcut saved", kind: "info" }));
      onDone();
    } catch (e) {
      const status = errorStatus(e);
      if (status === 409 && errorWord(e) !== "unsupported") {
        setFailed("That shortcut is used by another app — pick another.");
        void check.refetch();
      } else if (status === 400) {
        const why = errorMessage(e);
        setFailed(/other apps/i.test(why) ? why : `Not a valid shortcut. ${why}`.trim());
      } else setFailed(errorMessage(e) || "Couldn't save the shortcut. Try again.");
    }
  };

  let line: ReactNode = null;
  if (problem) line = <span className="text-destructive">{problem}</span>;
  else if (picked && !answer && check.isError) line = <span className="text-destructive">Couldn't check this shortcut. Try again.</span>;
  else if (picked && !answer)
    line = (
      <span className="inline-flex items-center gap-1.5">
        <Loader2 className="size-3.5 animate-spin" /> Checking…
      </span>
    );
  else if (verdict) line = <span className={verdict.good ? "text-foreground" : "text-destructive"}>{verdict.text}</span>;

  const keys = held.length ? held : shown;
  return (
    <div className={className}>
      <div
        ref={box}
        role="button"
        tabIndex={0}
        aria-label="Press the keys you want to use"
        onKeyDown={onKeyDown}
        // Only while modifiers are being held with nothing recorded yet: letting go of them after a combination was pressed leaves that combination showing.
        onKeyUp={(e) => {
          if (held.length && MODIFIER_KEYS.has(e.key)) setHeld(modifiersOf(e));
        }}
        onBlur={() => setHeld([])}
        className="flex min-h-12 items-center justify-center gap-2 rounded-md border border-dashed border-hairline-strong bg-sunken px-3 py-2 text-ui text-muted-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"
      >
        {keys.length ? (
          <>
            <Keys keys={keys} />
            {held.length ? <span>…</span> : null}
          </>
        ) : (
          "Press the keys you want to use"
        )}
      </div>
      <div aria-live="polite" className="mt-2 min-h-5 text-meta text-muted-foreground">
        {line}
      </div>
      {failed ? (
        <p role="alert" className="mt-1 text-meta text-destructive">
          {failed}
        </p>
      ) : null}
      <p className="mt-1 text-meta text-muted-foreground">If nothing shows up when you press keys, another app is using them.</p>
      <div className="mt-3 flex flex-wrap items-center gap-2">
        {!isDefault ? (
          <Button variant="ghost" size="sm" onClick={() => pick(DEFAULT_SHORTCUT, true)}>
            Use {DEFAULT_SHORTCUT}
          </Button>
        ) : null}
        <Button variant="ghost" size="sm" className="ml-auto" onClick={onDone}>
          Cancel
        </Button>
        <Button size="sm" disabled={!verdict?.good || saving} onClick={() => void commit()}>
          {saving ? <Loader2 className="animate-spin" /> : null} Save
        </Button>
      </div>
    </div>
  );
}

/** The Shortcut row on Settings: the keys in effect and a line saying whether they work, with Change beside them and the editor opening under the row. Input: none; it reads GET /settings. Output: the row. */
export function ShortcutRow() {
  const { data } = useSettingsQuery();
  const [editing, setEditing] = useState(false);
  const s = shortcutOf(data);
  return (
    <div>
      <div className="flex items-center justify-between gap-6 px-3.5 py-2.5">
        <div className="min-w-0">
          <div className="text-ui">Shortcut</div>
          <div className={`mt-0.5 text-meta ${s.status === "taken" ? "text-destructive" : "text-muted-foreground"}`}>{s.hint}</div>
        </div>
        <div className="flex shrink-0 items-center gap-2">
          {s.keys.length ? <Keys keys={s.keys} muted /> : null}
          {s.canChange && !editing ? (
            <Button variant="outline" size="sm" onClick={() => setEditing(true)}>
              {s.keys.length ? "Change" : "Choose a shortcut"}
            </Button>
          ) : null}
        </div>
      </div>
      {editing ? <ShortcutEditor current={data?.hotkey ?? ""} onDone={() => setEditing(false)} className="px-3.5 pb-3.5" /> : null}
    </div>
  );
}

/** How to open June, on the last setup screen: the keys to press, or why there are none with the way to pick some right under it. Input: none; it reads GET /settings. Output: the lines. */
export function OpenJune() {
  const { data } = useSettingsQuery();
  const [editing, setEditing] = useState(false);
  const s = shortcutOf(data);
  let line: ReactNode;
  if (s.keys.length && s.status !== "pending")
    line = (
      <span className="inline-flex flex-wrap items-center gap-1">
        Press <Keys keys={s.keys} /> from anywhere.
      </span>
    );
  else if (s.status === "taken") line = <span className="text-destructive">{s.hint}</span>;
  else if (s.status === "unsupported" || s.status === "pending") line = s.hint;
  else line = `Open it from ${ON_WINDOWS ? "the Start menu" : "your apps menu"} whenever you need it.`;
  return (
    <>
      {line}
      {s.canChange && !editing ? (
        <div className="mt-2">
          <Button variant="outline" size="sm" onClick={() => setEditing(true)}>
            {s.keys.length ? "Change shortcut" : "Choose a shortcut"}
          </Button>
        </div>
      ) : null}
      {editing ? <ShortcutEditor current={data?.hotkey ?? ""} onDone={() => setEditing(false)} className="mt-3" /> : null}
    </>
  );
}
