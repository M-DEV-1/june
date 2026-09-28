/** The read half of notice click-through. A click on a card in the hover leaves what it was about in the localStorage both windows share (see OPEN_AT_KEY and openNotice in src/main.tsx) and asks the daemon to open this window; this file is what this window does with that the moment it is shown — open the place named, select the row named on it if there was one, and clear the key so a later show does not jump there again unasked. */

import { useEffect } from "react";

import { OPEN_AT_KEY } from "../app/state";
import { ui, useAppDispatch, type Place } from "./store";

/** Every place a notice may name here, kept local rather than shared with the old window's own list (src/app/state.ts's PLACES), which has no "routines" — the two windows' Place types have diverged and each reads its own notices against its own screens. */
const places: Place[] = ["chats", "tasks", "days", "meetings", "settings", "routines"];

/** Reads a notice target out of what the hover wrote. Input: the text stored under OPEN_AT_KEY, or null when nothing is stored. Output: the target, or undefined when nothing was stored, the text is not JSON, or its place names no screen this window has. An empty id means the notice pointed at a screen but no row on it. */
export function parseOpenAt(text: string | null): { place: Place; id?: string } | undefined {
  if (!text) return undefined;
  let raw: unknown;
  try {
    raw = JSON.parse(text);
  } catch {
    return undefined;
  }
  if (typeof raw !== "object" || raw === null) return undefined;
  const { place, id } = raw as { place?: unknown; id?: unknown };
  if (typeof place !== "string" || !places.includes(place as Place)) return undefined;
  return { place: place as Place, id: typeof id === "string" && id !== "" ? id : undefined };
}

/** Opens this window at what a clicked notice named, once, the moment it is shown. Input: none. Output: nothing — call once near the top of App. */
export function useOpenAtOnShow(): void {
  const dispatch = useAppDispatch();
  useEffect(() => {
    const target = parseOpenAt(localStorage.getItem(OPEN_AT_KEY));
    localStorage.removeItem(OPEN_AT_KEY);
    if (!target) return;
    dispatch(ui.placeShown(target.place));
    if (!target.id) return;
    switch (target.place) {
      case "chats":
        dispatch(ui.conversationOpened(target.id));
        break;
      case "tasks":
        dispatch(ui.taskOpened(target.id));
        break;
      case "days":
        dispatch(ui.dayOpened(target.id));
        break;
      case "meetings":
        dispatch(ui.meetingOpened(target.id));
        break;
      case "routines":
        dispatch(ui.routineOpened(target.id));
        break;
      case "settings":
        break;
    }
  }, [dispatch]);
}
