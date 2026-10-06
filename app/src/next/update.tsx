/** A newer June: the strip across the top of the window that says one is out and installs it in one click, and the row in Settings that says which version this is. The daemon checks once a day and does the download, the SHA256SUMS check and the install itself (see internal/update); the window only asks and shows how far it has got, off the "update" events. Where June cannot replace itself — on Linux, or a build with no version — both link to the release page instead. */

import { useState } from "react";
import { ArrowUpRight, Loader2, X } from "lucide-react";

import { Button } from "@/components/ui/button";
import { errorMessage, errorStatus, errorWord, useCheckUpdateMutation, useInstallUpdateMutation, useUpdateQuery, type UpdateView } from "./api";
import { useOpenLink } from "./parts";
import { useAppSelector, type UpdateProgress } from "./store";

/** The localStorage key holding the version whose strip was closed, so closing it hides that release and not the next one. */
const DISMISSED_KEY = "june.update.dismissed";

/** How often the window reads GET /update again while it stays open. The daemon checks GitHub once a day; a window left open for days should notice within a few hours of it. */
const UPDATE_POLL_MS = 3 * 60 * 60 * 1000;

function readDismissed(): string {
  try {
    return localStorage.getItem(DISMISSED_KEY) ?? "";
  } catch {
    return "";
  }
}

/** What the Install buttons say while a meeting is being recorded. */
const AFTER_RECORDING = "Install it once the meeting recording is over.";

/** What the install has got to, in words. Input: the last "update" event, the update as GET /update reads it, and whether the POST is still in flight. Output: the line, and the share done for the bar (undefined when there is no bar to draw). */
function installLine(p: UpdateProgress | undefined, u: UpdateView, starting: boolean): { text: string; pct?: number } | undefined {
  if (p?.text === "downloading" || (!p && u.state === "downloading")) {
    const pct = p && p.total > 0 ? Math.min(100, Math.round((p.done / p.total) * 100)) : undefined;
    return { text: `Downloading June ${u.latest}…${pct !== undefined ? ` ${pct}%` : ""}`, pct: pct ?? 0 };
  }
  if (p?.text === "verifying" || (!p && u.state === "ready")) return { text: "Checking the download…", pct: 100 };
  if (p?.text === "installing" || (!p && u.state === "installing")) return { text: "Installing — June will restart in a moment…" };
  if (starting) return { text: "Starting the download…", pct: 0 };
  return undefined;
}

/** Why an install did not start, in words. Input: what POST /update rejected with. Output: the sentence. */
function refusal(e: unknown): string {
  const status = errorStatus(e);
  if (status === undefined) return "June isn't answering. Try again in a moment.";
  switch (errorWord(e)) {
    case "dev_build":
      return "A development build doesn't update itself.";
    case "no_update":
      return "June is already up to date.";
    case "cannot_install":
      return "June can't install this one itself — download it from the release page.";
    case "recording":
    case "processing":
    case "downloading":
      // The daemon's sentence names what installing would cut short; a meeting's write-up is one this window has no way to see coming.
      return errorMessage(e) || "June is in the middle of something that installing would cut short. Try again in a few minutes.";
  }
  if (status === 404) return "This version of June can't update itself.";
  return errorMessage(e) || "The update couldn't start.";
}

/** The strip across the top of the window. Input: none. Output: the strip, or nothing while June is up to date or the strip for this release was closed. */
export function UpdateBanner() {
  const { data: u } = useUpdateQuery(undefined, { pollingInterval: UPDATE_POLL_MS });
  const progress = useAppSelector((s) => s.setup.update);
  // The installer quits June, which ends a meeting recording mid-call. The daemon is what refuses (update.Options.Blocker, which also covers a write-up and a download); this window only knows of a recording whose start it heard on the stream, and where it does, the button says so beforehand rather than after.
  const recording = useAppSelector((s) => Boolean(s.progress.recording));
  const [install, { isLoading: starting }] = useInstallUpdateMutation();
  const openLink = useOpenLink();
  const [dismissed, setDismissed] = useState(readDismissed);
  const [refused, setRefused] = useState("");

  if (!u?.available) return null;
  const line = installLine(progress, u, starting);
  const failed = !line && (u.state === "failed" || Boolean(refused));
  if (!line && dismissed === u.latest) return null;

  const close = () => {
    setDismissed(u.latest);
    try {
      localStorage.setItem(DISMISSED_KEY, u.latest);
    } catch {
      /* storage blocked */
    }
  };

  const go = async () => {
    setRefused("");
    try {
      await install().unwrap();
    } catch (e) {
      setRefused(refusal(e));
    }
  };

  return (
    <div role="status" className="flex shrink-0 flex-wrap items-center gap-x-3 gap-y-1.5 border-b bg-card px-4 py-2 text-ui">
      {line ? (
        <>
          <Loader2 aria-hidden className="size-3.5 shrink-0 animate-spin text-muted-foreground" />
          <span className="tabular-nums">{line.text}</span>
          {line.pct !== undefined ? (
            <div className="h-1 w-32 overflow-hidden rounded-full bg-muted">
              <div className="h-full rounded-full bg-primary transition-[width] duration-300" style={{ width: `${line.pct}%` }} />
            </div>
          ) : null}
        </>
      ) : failed ? (
        <>
          <span className="text-destructive">
            June {u.latest} didn't install{refused || u.error ? `: ${refused || u.error}` : "."}
            {u.can_install && recording && !refused ? <span className="text-muted-foreground"> {AFTER_RECORDING}</span> : null}
          </span>
          <div className="ml-auto flex items-center gap-1">
            <Button variant="ghost" size="sm" onClick={() => openLink(u.release_url)}>
              Download it yourself <ArrowUpRight />
            </Button>
            {u.can_install ? (
              <Button size="sm" disabled={recording} onClick={() => void go()}>
                Try again
              </Button>
            ) : null}
            <Button variant="ghost" size="icon-sm" aria-label="Not now" onClick={close}>
              <X />
            </Button>
          </div>
        </>
      ) : (
        <>
          <span>
            June {u.latest} is out<span className="text-muted-foreground"> — you have {u.current}.{u.can_install && recording ? ` ${AFTER_RECORDING}` : ""}</span>
          </span>
          <div className="ml-auto flex items-center gap-1">
            <Button variant="ghost" size="sm" onClick={() => openLink(u.release_url)}>
              What's new <ArrowUpRight />
            </Button>
            {u.can_install ? (
              <Button size="sm" disabled={recording} onClick={() => void go()}>
                Install and restart
              </Button>
            ) : (
              <Button size="sm" onClick={() => openLink(u.release_url)}>
                Get it
              </Button>
            )}
            <Button variant="ghost" size="icon-sm" aria-label="Not now" onClick={close}>
              <X />
            </Button>
          </div>
        </>
      )}
    </div>
  );
}

/** The version row at the foot of Settings. Input: the version GET /settings reports, for a daemon too old to answer GET /update. Output: the row's right-hand side. */
export function UpdateStatus({ version }: { version: string }) {
  const { data: u, isError } = useUpdateQuery(undefined, { pollingInterval: UPDATE_POLL_MS });
  const progress = useAppSelector((s) => s.setup.update);
  const recording = useAppSelector((s) => Boolean(s.progress.recording));
  const [install, { isLoading: starting }] = useInstallUpdateMutation();
  const [check, { isLoading: checking }] = useCheckUpdateMutation();
  const openLink = useOpenLink();
  const [refused, setRefused] = useState("");
  const current = u?.current || version;

  if (isError || !u) return <span className="text-ui text-muted-foreground">{current || "unknown"}</span>;
  if (current === "dev" || current === "") return <span className="text-ui text-muted-foreground">Development build — it doesn't update itself</span>;
  if (!u.available) {
    return (
      <div className="flex flex-col items-end gap-1">
        <div className="flex items-center gap-2">
          <span className="text-ui text-muted-foreground">{current} · up to date</span>
          <Button variant="ghost" size="sm" disabled={checking} onClick={() => void check()}>
            {checking ? <Loader2 className="animate-spin" /> : null} Check now
          </Button>
        </div>
        {u.state === "failed" && u.error ? <span className="text-meta text-destructive">Couldn't check: {u.error}</span> : null}
      </div>
    );
  }
  const line = installLine(progress, u, starting);
  if (line) return <span className="text-ui text-muted-foreground tabular-nums">{line.text}</span>;

  const go = async () => {
    setRefused("");
    try {
      await install().unwrap();
    } catch (e) {
      setRefused(refusal(e));
    }
  };

  return (
    <div className="flex flex-col items-end gap-1">
      <div className="flex items-center gap-2">
        <span className="text-ui">
          {current} · <span className="text-foreground">{u.latest} is out</span>
        </span>
        {u.can_install ? (
          <Button size="sm" disabled={recording} onClick={() => void go()}>
            Install
          </Button>
        ) : (
          <Button size="sm" variant="outline" onClick={() => openLink(u.release_url)}>
            Get it <ArrowUpRight />
          </Button>
        )}
      </div>
      {refused || u.state === "failed" ? <span className="text-meta text-destructive">{refused || u.error || "The last try didn't install."}</span> : null}
      {u.can_install && recording && !refused ? <span className="text-meta text-muted-foreground">{AFTER_RECORDING}</span> : null}
    </div>
  );
}
