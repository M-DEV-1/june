/** The optional local features as cards — voice typing, who said what, smarter memory search, on-device summaries — each saying what it costs to download on this machine, what it will run on, and how far its download has got. The same list is a step of first-run setup and a section of Settings; only Settings offers Remove and Restart, since setup restarts June itself once it is finished. */

import { useState } from "react";
import { Check, Loader2 } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "./alert-dialog";
import {
  errorMessage,
  errorStatus,
  errorWord,
  useCancelFeatureMutation,
  useComponentsQuery,
  useInstallFeatureMutation,
  useRemoveFeatureMutation,
  useRestartJuneMutation,
  type Feature,
  type Platform,
} from "./api";
import { bytes, downloadLine } from "./format";
import { Group, ON_WINDOWS } from "./parts";
import { setupUi, useAppDispatch, useAppSelector, type FeatureProgress } from "./store";

/** What each feature is for, in a sentence a person who has never heard of whisper.cpp can act on. The daemon names a feature but does not sell it, and a title alone does not say why 2 GB is worth downloading. */
const WHAT_FOR: Record<string, string> = {
  transcribe: "Type by talking, and get written transcripts of your meetings. Speech is turned into text on this PC.",
  speakers: "Marks who was speaking in each meeting transcript.",
  memory: "Finds what you are asking about by meaning, not only by the exact words.",
  summaries: "Writes meeting summaries on this PC instead of sending them to an online AI.",
};

/** The room the daemon keeps free beyond a download (diskMargin in internal/components/install.go). The card only warns with it: what is already downloaded and what an archive unpacks to are the daemon's to count, and its 507 is what refuses. */
const SPARE_BYTES = 500 * 1024 * 1024;

/** What a restart right now would cut short that this window knows of: "recording" while a meeting is being recorded, "downloading" while a local feature is queued or downloading, "" when neither. The daemon refuses a key change for both and finishing setup puts its restart off for them (internal/ipc/setup.go); the window says so before the click rather than after. A meeting still being written up is the daemon's alone to know, and comes back as its 409. */
export type RestartBlocker = "" | "recording" | "downloading";

export function useRestartBlocker(): RestartBlocker {
  const recording = useAppSelector((s) => s.progress.recording);
  const { data } = useComponentsQuery();
  if (recording) return "recording";
  if (data?.features.some((f) => f.state === "queued" || f.state === "installing")) return "downloading";
  return "";
}

/** Why a button that restarts June is held back. Input: what is in the way, and what the button does as the start of a sentence ("Saving a key"), left out for a button that is plainly a restart. Output: the sentence, or "" when nothing is in the way. */
export function restartBlockedLine(blocker: RestartBlocker, doing = ""): string {
  const lead = doing ? `${doing} restarts June, which` : "Restarting now";
  if (blocker === "recording") return `${lead} would stop the meeting recording. Try again once the meeting is over.`;
  if (blocker === "downloading") return `${lead} would stop the local features that are downloading. Wait for them to finish, or cancel them first.`;
  return "";
}

/** What Windows' Smart App Control means for these downloads, in words. Input: its state off GET /components. Output: the warning, or "" when it is off, unknown, or not Windows. The programs these features download are not signed yet, and Smart App Control judges an unsigned program by Microsoft's reputation for it, which cannot be read beforehand, so the line says likely, not certain, and leaves Set up enabled; with it on, the daemon runs each program before the big downloads (tryStart in internal/components/install.go), so a block shows up early rather than after gigabytes. It does not tell anyone to turn the protection off. */
function smartAppControlLine(state: Platform["smart_app_control"]): string {
  if (state === "on") return "Windows' Smart App Control is on, and will most likely block the programs these features download. June tries each program before the big downloads, so a block shows up early. June works without them.";
  if (state === "evaluation") return "Windows' Smart App Control is checking new apps on this PC and may block the programs these features download; if a set-up fails, that may be why. June works without them.";
  return "";
}

/** What a feature will run on, in one line. Input: the build the daemon picked for this machine and what it found. Output: the line, or "" when there is nothing worth saying. */
function runsOn(variant: string, platform: Platform): string {
  // Memory under a gigabyte is a card sharing the system's own, where a figure would only read as "(0 GB)".
  const gpu = platform.gpu?.name ? `${platform.gpu.name}${platform.gpu.vram_mb >= 1024 ? ` (${Math.round(platform.gpu.vram_mb / 1024)} GB)` : ""}` : "";
  if (variant === "cuda" || variant === "vulkan") return gpu ? `Uses your ${gpu}` : "Uses your graphics card";
  if (variant === "cpu") return "Runs on your processor";
  return "";
}

/** What a download in flight is doing, in words. Input: its progress off the stream, or undefined before the first event. Output: the line under the bar. */
function stageLine(p?: FeatureProgress): string {
  switch (p?.text) {
    case "downloading":
      return downloadLine(p.done, p.total, p.bps) || "Downloading…";
    case "verifying":
      return "Checking the download…";
    case "extracting":
      return "Unpacking…";
    case "testing":
      return "Making sure it works…";
    default:
      return "Starting…";
  }
}

/** Why a click on Set up, Cancel, Remove or Restart did not go through, in words. Input: the caught refusal. Output: the sentence: the daemon's own when it sent one, which names the feature and the numbers (the disk space short, the memory missing, the feature it needs first), and otherwise one of these. */
function refusal(e: unknown): string {
  const status = errorStatus(e);
  if (status === undefined) return "June isn't answering. Try again in a moment.";
  const said = errorMessage(e);
  if (said) return said.endsWith(".") ? said : `${said}.`;
  const word = errorWord(e);
  if (word === "recording") return "A meeting is being recorded. Restart June once it's over.";
  if (status === 507) return "There isn't enough free space on this PC for it.";
  if (status === 409) return "It's in use right now — finish the meeting or dictation first, then try again.";
  if (status === 404) return "This version of June can't download that yet.";
  return "That didn't work. Try again in a moment.";
}

/** One feature's card. Input: the feature, every feature (for the names of the ones it builds on), the machine, where its download has got to, whether this is Settings (which offers Remove), and what Set up, Cancel and Remove do. Output: the row. */
function FeatureRow({
  f,
  all,
  platform,
  progress,
  manage,
  error,
  onInstall,
  onCancel,
  onRemove,
}: {
  f: Feature;
  all: Feature[];
  platform: Platform;
  progress?: FeatureProgress;
  manage: boolean;
  error?: string;
  onInstall: () => void;
  onCancel: () => void;
  onRemove: () => void;
}) {
  // GET /components decides whether a download is under way, and the stream only how far it has got: progress left behind by an ending this window never heard (the stream dropped across it) must not hold a finished card in its download state.
  const busy = f.state === "queued" || f.state === "installing";
  const ready = f.state === "installed" || f.state === "external";
  // A feature that builds on another sets that one up too, and the card says so before the click rather than after.
  const missing = f.requires.map((id) => all.find((x) => x.id === id)).filter((x): x is Feature => x !== undefined && x.state !== "installed" && x.state !== "external");
  const tight = platform.free_bytes > 0 && platform.free_bytes < f.download_bytes + SPARE_BYTES;
  const pct = progress && progress.total > 0 ? Math.min(100, Math.round((progress.done / progress.total) * 100)) : 0;
  const line = [ready ? `${bytes(f.disk_bytes)} on this PC` : `${bytes(f.download_bytes)} download`, runsOn(f.variant, platform)].filter(Boolean).join(" · ");

  return (
    <div className="px-4 py-3.5">
      <div className="flex items-start justify-between gap-4">
        <div className="min-w-0">
          <div className="flex items-center gap-1.5 text-ui font-medium">
            {f.title}
            {ready ? <Check aria-hidden className="size-3.5 text-primary" /> : null}
          </div>
          {WHAT_FOR[f.id] ? <p className="mt-0.5 text-meta text-muted-foreground">{WHAT_FOR[f.id]}</p> : null}
          <p className="mt-1 text-meta text-muted-foreground">{line}</p>
        </div>
        <div className="flex shrink-0 items-center gap-1.5">
          {busy ? (
            <Button variant="ghost" size="sm" onClick={onCancel}>
              Cancel
            </Button>
          ) : f.state === "external" ? (
            <span className="text-meta text-muted-foreground" title="Its files were put there by hand, so June uses them but leaves them alone.">
              Already on this PC
            </span>
          ) : f.state === "installed" ? (
            manage ? (
              <Button variant="ghost" size="sm" onClick={onRemove}>
                Remove
              </Button>
            ) : (
              <span className="text-meta text-muted-foreground">Ready</span>
            )
          ) : (
            <Button variant={manage ? "outline" : "default"} size="sm" onClick={onInstall}>
              {f.state === "failed" ? "Try again" : f.state === "update_available" ? "Update" : "Set up"}
            </Button>
          )}
        </div>
      </div>
      {busy ? (
        <div className="mt-3">
          <div className="h-1 w-full overflow-hidden rounded-full bg-muted" role="progressbar" aria-label={`${f.title} download`} aria-valuemin={0} aria-valuemax={100} aria-valuenow={pct}>
            <div className={`h-full rounded-full bg-primary transition-[width] duration-300 ${progress?.text === "downloading" ? "" : "animate-pulse"}`} style={{ width: `${progress?.text === "downloading" ? pct : progress ? 100 : 0}%` }} />
          </div>
          <p className="mt-1.5 text-meta text-muted-foreground tabular-nums">{f.state === "queued" && !progress ? "Waiting for the download before it to finish…" : stageLine(progress)}</p>
        </div>
      ) : null}
      {!busy && !ready && missing.length ? <p className="mt-1.5 text-meta text-muted-foreground">Also sets up {missing.map((m) => m.title).join(" and ")}.</p> : null}
      {!busy && !ready && tight && !error ? (
        <p className="mt-1.5 text-meta text-muted-foreground">
          This PC is low on space: the download is {bytes(f.download_bytes)} and there's {bytes(platform.free_bytes)} free.
        </p>
      ) : null}
      {f.state === "failed" && f.error && !busy ? <p className="mt-1.5 text-meta text-destructive">{f.error}</p> : null}
      {error ? <p className="mt-1.5 text-meta text-destructive">{error}</p> : null}
    </div>
  );
}

/** The feature cards. Input: whether this is Settings, which offers Remove and a restart once something needs one; setup restarts June itself when it is finished. Output: the group, or a line saying the daemon offers none. */
export function FeatureList({ manage = false }: { manage?: boolean }) {
  const dispatch = useAppDispatch();
  const { data, isError, isLoading } = useComponentsQuery();
  const progress = useAppSelector((s) => s.setup.features);
  const [install] = useInstallFeatureMutation();
  const [cancel] = useCancelFeatureMutation();
  const [remove] = useRemoveFeatureMutation();
  const [restart, { isLoading: restarting }] = useRestartJuneMutation();
  const blocker = useRestartBlocker();
  // A refusal is said on the card it belongs to, not in the rail's one notice line, which a person looking at the card would never see.
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [removing, setRemoving] = useState<Feature | undefined>(undefined);
  // The confirmation keeps naming the feature while it fades out, the way the delete-a-chat one does in App.tsx: removing goes undefined the instant it is answered.
  const [named, setNamed] = useState<Feature | undefined>(undefined);
  if (removing && removing !== named) setNamed(removing);

  if (isLoading) return <p className="text-ui text-muted-foreground">Looking at this PC…</p>;
  if (isError || !data) return <p className="text-ui text-muted-foreground">This version of June has no local features to offer.</p>;
  const features = data.features;
  if (!features.length) return <p className="text-ui text-muted-foreground">No local features are offered on this computer.</p>;

  const said = (id: string, text?: string) => setErrors((was) => ({ ...was, [id]: text ?? "" }));
  const blocked = smartAppControlLine(data.platform.smart_app_control);

  /** Queues a feature after whatever it builds on, so "Who said what" sets voice typing up first rather than failing for want of it. */
  const setUp = async (f: Feature) => {
    said(f.id);
    const first = f.requires.filter((id) => {
      const r = features.find((x) => x.id === id);
      return r && (r.state === "not_installed" || r.state === "failed");
    });
    try {
      for (const id of [...first, f.id]) await install(id).unwrap();
    } catch (e) {
      said(f.id, refusal(e));
    }
  };

  const stop = async (f: Feature) => {
    try {
      await cancel(f.id).unwrap();
      dispatch(setupUi.featureEvent({ id: f.id, type: "component", text: "cancelled" }));
    } catch (e) {
      said(f.id, refusal(e));
    }
  };

  const confirmRemove = async () => {
    const f = removing;
    setRemoving(undefined);
    if (!f) return;
    said(f.id);
    try {
      await remove(f.id).unwrap();
    } catch (e) {
      said(f.id, refusal(e));
    }
  };

  const restartNow = async () => {
    said("restart");
    try {
      await restart().unwrap();
      dispatch(setupUi.restartBegan("restart"));
    } catch (e) {
      // The daemon's own sentence for every refusal, which names what a restart would cut short — a write-up included, which this window cannot see coming. Advice to quit and reopen is only for a failure that said nothing, since quitting during a write-up loses the meeting's notes.
      const status = errorStatus(e);
      said("restart", !errorMessage(e) && status !== undefined && status >= 500 ? `June couldn't restart itself. Quit it from its tray icon${ON_WINDOWS ? " (by the clock; click ^ if you don't see it)" : ""}, then open it again.` : refusal(e));
      // The daemon also announces this failure on the stream, and it is said here already.
      if (errorWord(e) === "restart_failed") dispatch(setupUi.restartFailureSaid());
    }
  };

  return (
    <>
      {blocked ? (
        <p role="note" className="mb-3 rounded-lg border border-destructive/30 bg-card px-4 py-3 text-meta text-foreground">
          {blocked}
        </p>
      ) : null}
      <Group>
        <div className="divide-y">
          {features.map((f) => (
            <FeatureRow
              key={f.id}
              f={f}
              all={features}
              platform={data.platform}
              progress={progress[f.id]}
              manage={manage}
              error={errors[f.id]}
              onInstall={() => void setUp(f)}
              onCancel={() => void stop(f)}
              onRemove={() => setRemoving(f)}
            />
          ))}
        </div>
      </Group>
      {manage && data.restart_pending ? (
        <div className="mt-3 flex items-center justify-between gap-4 rounded-lg border bg-card px-4 py-3">
          <div className="min-w-0">
            {/* One sentence for a feature added and one taken away alike: the daemon says only that a restart is waiting, not which change it is waiting for. */}
            <p className="text-ui">Restart June so the change takes effect.</p>
            {/* POST /restart refuses a recording, a meeting's write-up and a download alike, and says which; the button is held back for the two this window can see coming, so the person hears it before the click rather than after. */}
            {blocker ? <p className="mt-0.5 text-meta text-muted-foreground">{restartBlockedLine(blocker)}</p> : null}
          </div>
          <Button size="sm" className="shrink-0" disabled={restarting || Boolean(blocker)} onClick={() => void restartNow()}>
            {restarting ? <Loader2 className="animate-spin" /> : null} Restart now
          </Button>
        </div>
      ) : null}
      {errors.restart ? <p className="mt-1.5 text-meta text-destructive">{errors.restart}</p> : null}
      <AlertDialog open={Boolean(removing)} onOpenChange={(open) => !open && setRemoving(undefined)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Remove {named?.title}?</AlertDialogTitle>
            <AlertDialogDescription>This deletes {named ? bytes(named.disk_bytes) : "its files"} from this PC. You can set it up again any time.</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Keep it</AlertDialogCancel>
            <AlertDialogAction onClick={() => void confirmRemove()}>Remove</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}

/** A feature still on its way: its title, how far its download has got (undefined before the first progress event), and whether it only starts working once June restarts. */
export type UnderWay = { title: string; pct?: number; needsRestart: boolean };

/** Whether any feature is downloading or waiting to, for the last setup screen to say what is still on its way. Input: none. Output: one entry per feature still under way. */
export function useFeaturesUnderWay(): UnderWay[] {
  const { data } = useComponentsQuery();
  const progress = useAppSelector((s) => s.setup.features);
  return (data?.features ?? [])
    .filter((f) => f.state === "queued" || f.state === "installing")
    .map((f) => {
      const p = progress[f.id];
      return { title: f.title, pct: p && p.total > 0 ? Math.round((p.done / p.total) * 100) : undefined, needsRestart: f.needs_restart };
    });
}
