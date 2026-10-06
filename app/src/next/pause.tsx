/** Pausing what June watches, and saying so wherever the person looks: the control beside June's face at the top of the rail, which pauses for a quarter of an hour, an hour or until resumed, and the strip across the top of every screen while June is paused. Settings keeps its own on/off switch, which is the until-resumed pause. */

import { useEffect } from "react";
import { Eye, EyeOff, Pause, Play } from "lucide-react";

import { Button } from "@/components/ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { usePauseForMutation, useSetCaptureMutation, useTrackerQuery } from "./api";
import { hhmm } from "./format";
import { ui, useAppDispatch } from "./store";

/** How often the window asks whether June is paused. The tray pauses and resumes too, and nothing on the event stream says so; a minute is soon enough for a line that says what June is doing. */
const STATUS_POLL_MS = 60_000;

/** The pauses on offer, in minutes, 0 for one that lasts until resumed. A pause with an end is the one that cannot be forgotten about for days. */
const PAUSES = [
  { minutes: 15, label: "Pause for 15 minutes" },
  { minutes: 60, label: "Pause for 1 hour" },
  { minutes: 0, label: "Pause until I resume" },
];

/** The longest delay setTimeout honours; a longer one fires straight away. */
const MAX_TIMER_MS = 2 ** 31 - 1;

/** Whether June is paused, and until when. Input: none. Output: paused, and the RFC3339 moment a timed pause ends ("" for one that lasts until resumed, and while watching). */
export function usePaused(): { paused: boolean; until: string } {
  const { data, refetch } = useTrackerQuery(undefined, { pollingInterval: STATUS_POLL_MS });
  const paused = Boolean(data?.paused);
  const until = paused ? (data?.paused_until ?? "") : "";
  // A timed pause ends on the daemon's clock, and "paused until 14:30" should not stay up for most of a poll after 14:30.
  useEffect(() => {
    const at = Date.parse(until);
    if (Number.isNaN(at)) return;
    const id = setTimeout(() => void refetch(), Math.min(MAX_TIMER_MS, Math.max(0, at - Date.now()) + 1000));
    return () => clearTimeout(id);
  }, [until, refetch]);
  return { paused, until };
}

/** The words for a pause. Input: when it ends, "" for one that lasts until resumed. Output: "paused until 14:30", or "paused". */
export function pausedWords(until: string): string {
  return until ? `paused until ${hhmm(until)}` : "paused";
}

/** The button beside June's face: a menu of pauses while June watches, and Resume while it is paused. Input: whether it is paused, and when a timed pause ends. Output: the button. In the rail folded to icons the face and its words are hidden and this is all that is left, so there it shows an open eye while June watches and a shut one on a filled square while it is paused; its name says the state as well as the action for the same reason. */
export function PauseControl({ paused, until = "" }: { paused: boolean; until?: string }) {
  const dispatch = useAppDispatch();
  const [pauseFor] = usePauseForMutation();
  const [setCapture] = useSetCaptureMutation();
  const said = (text: string) => () => dispatch(ui.noticed({ text, kind: "error" }));
  if (paused) {
    const name = `June is ${pausedWords(until)}. Resume watching`;
    return (
      <Button
        variant="ghost"
        size="icon-sm"
        aria-label={name}
        title={name}
        className="text-muted-foreground hover:text-foreground group-data-[collapsible=icon]:bg-sidebar-accent group-data-[collapsible=icon]:text-foreground"
        onClick={() => void setCapture(true).unwrap().catch(said("Could not start watching"))}
      >
        <Play className="group-data-[collapsible=icon]:hidden" />
        <EyeOff className="hidden group-data-[collapsible=icon]:block" />
      </Button>
    );
  }
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="ghost" size="icon-sm" aria-label="June is watching. Pause watching" title="June is watching. Pause watching" className="text-muted-foreground hover:text-foreground">
          <Pause className="group-data-[collapsible=icon]:hidden" />
          <Eye className="hidden group-data-[collapsible=icon]:block" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="start">
        {PAUSES.map((p) => (
          <DropdownMenuItem key={p.minutes} onClick={() => void pauseFor(p.minutes).unwrap().catch(said("Could not pause watching"))}>
            {p.label}
          </DropdownMenuItem>
        ))}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

/** The strip across the top of every screen while June is paused, so a pause is never mistaken for June remembering nothing. Input: none. Output: the strip, or nothing while June watches. */
export function PausedBanner() {
  const dispatch = useAppDispatch();
  const { paused, until } = usePaused();
  const [setCapture, { isLoading }] = useSetCaptureMutation();
  if (!paused) return null;
  return (
    <div role="status" className="flex shrink-0 flex-wrap items-center gap-x-3 gap-y-1.5 border-b bg-card px-4 py-2 text-ui">
      <span>
        June is {pausedWords(until)}
        <span className="text-muted-foreground">
          {until ? " — it isn't watching your screen, and starts again by itself." : " — it isn't watching your screen, so it won't remember what you do until you resume."}
        </span>
      </span>
      <Button size="sm" className="ml-auto" disabled={isLoading} onClick={() => void setCapture(true).unwrap().catch(() => dispatch(ui.noticed({ text: "Could not start watching", kind: "error" })))}>
        Resume now
      </Button>
    </div>
  );
}
