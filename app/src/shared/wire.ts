/** The daemon's event stream, declared once for both clients that read it: the hover (src/daemon.ts, src/state.ts) and the React window (src/next/api.ts, src/next/store.ts). Holds the shapes of what arrives on it and the one reader that opens it and reconnects it. */

/** One thing June read to answer with, carried both on a stored turn and on the answer event. Mirrors ipc.EvidenceItem. */
export type Evidence = { title: string; meta: string; body: string };

/** One round's tokens, filed under the model that served it. Mirrors actjob.Usage. */
export type ModelUsage = { model: string; input: number; cached: number; output: number };

/** What a whole job has cost: the rounds it took, the tokens on each side, and the same counts again per model, so two brains can be compared on the same task. Mirrors actjob.Spend. */
export type Spend = { rounds: number; input: number; cached: number; output: number; by_model?: Record<string, ModelUsage> };

/** One of June's own moments, sent by the daemon rather than asked for: the morning brief, the evening close, a meeting prep, a task or routine raised on its own. The shape is fixed by the Go side, see internal/ipc/notice.go.
 * title is the card's bold first line and body the few lines under it; body is the routine's or task's own text for those two kinds (see internal/proactive/routine.go and proactive.go).
 * place names the app window's screen a click opens ("tasks", "days") and id the row to select there, both empty when the moment points at nothing in particular.
 * kind is "task", "routine", "brief", "close", "meeting" or "note".
 * action and until are empty on a notice arriving fresh, and set once the user has pressed Done or a snooze button on the desktop notification it was also posted as: action is "snoozed" or "done", and until is the RFC 3339 moment a snoozed notice comes back.
 */
export type Notice = {
  title: string;
  body: string;
  place: string;
  id: string;
  kind: string;
  action?: string;
  until?: string;
  /** The buttons this notice can answer, named by the daemon rather than guessed by the window: a task and the stale-task question carry their own, and anything with nothing to complete carries only Open (see openOnlyActions and noticeActions in internal/proactive). key is what goes back to POST /notices/{kind}/{id}/action, label is what the button reads. */
  actions?: { key: string; label: string }[];
  /** The RFC 3339 moment a question stops being answerable, set only on a notice that asked one. The daemon's goroutine waiting on the answer gives up then, so each window's card takes itself down at that moment rather than leaving a button that answers "Could not do that". */
  expires?: string;
};

/** One message off the daemon's SSE stream.
 * The first five types belong to an ask; "dictation" carries a finished transcript; "heard", "said", "state" and "level" belong to a live voice session, and "level" also to first-run setup's microphone test (id "mic-test"); "notice" is June speaking first; "act" is one line of a computer-use job's progress; "overlay" and "window" tell the on-screen accessories and the app window what to do; "recording" and "dreaming" say ("on" or "off") that a meeting is being captured or the nightly run is under way; "component" is one step of a local feature's download (id is the feature, text the stage) and "update" one step of installing a newer June, or with text "available" the daily check finding one; "lifecycle" is the daemon itself restarting ("restarting") or failing to ("restart_failed"), see announce in cmd/lifecycle.go.
 * id is the ask's own id, or for "act" the job's id, which is how a message is tied to the thing that caused it.
 */
export type DaemonEvent = {
  id: string;
  type: "status" | "tool" | "answer" | "done" | "error" | "dictation" | "heard" | "said" | "state" | "level" | "notice" | "act" | "overlay" | "window" | "recording" | "dreaming" | "component" | "update" | "lifecycle";
  text?: string;
  /** Carried on a "tool" event (a short summary of what that call is doing or found), on an "act" event (the job's actjob.Event as JSON, see parseActDetail in shared/job.ts) on a "level" event (the session's mic/speaker amplitude as JSON, {"mic":0-1,"speaker":0-1}, see waveform.ts's renderLevelEvent), on a "component" event ({"file","done","total","bps"}), on an "update" event ({"done","total"}) and on a "lifecycle" event ({"reopen":bool} on "restarting", saying whether the replacement shows its window; {"error":string} on "restart_failed"). */
  detail?: string;
  /** Only meaningful on the after-call "tool" event: true when that tool call's result was an error, so a step closes as failed instead of done. */
  failed?: boolean;
  evidence?: Evidence[];
  /** Only carried on the "answer" event. */
  conversation_id?: string;
  /** Only carried on a "notice" event: the whole card. */
  notice?: Notice;
};

/** How long a dropped stream waits before reconnecting, in milliseconds, and how much random extra is added on top. The jitter is there because every open window drops at the same instant when the daemon dies, and without it they all reconnect and refetch on the same tick for as long as it flaps. */
const RETRY_MS = 2000;
const RETRY_JITTER_MS = 1000;

/** Opens the daemon's SSE stream and forwards each parsed message to onEvent, reconnecting after a drop.
 * Input: the daemon's base URL; token, which gives the IPC token to connect with and is told whether this is a reconnect, so a caller can read a fresh token from a daemon that restarted; a callback for each event; and a callback for the stream opening again after it had dropped, which is the only signal a window gets that a daemon it had lost is answering again.
 * Output: a stop function that closes the stream for good.
 * The token goes in the query string because an EventSource cannot set headers, which is why the daemon accepts it there as well (see requireIPCToken in cmd/ipc.go).
 */
export function openStream(
  base: string,
  token: (retry: boolean) => Promise<string | undefined> | string | undefined,
  onEvent: (ev: DaemonEvent) => void,
  onReopen?: () => void,
): () => void {
  let stopped = false;
  let source: EventSource | undefined;
  // Whether the stream has failed since it was last open. Set on every error, including each failed retry, so it is still true whenever a later attempt finally succeeds.
  let dropped = false;

  async function connect(retry: boolean): Promise<void> {
    if (stopped) return;
    const t = await token(retry);
    if (stopped) return;
    source = new EventSource(t ? `${base}/events?token=${encodeURIComponent(t)}` : `${base}/events`);
    source.onopen = () => {
      // Only a stream that had dropped is a recovery worth reporting; the first open is not.
      if (!dropped) return;
      dropped = false;
      onReopen?.();
    };
    source.onmessage = (e: MessageEvent) => {
      try {
        onEvent(JSON.parse(e.data) as DaemonEvent);
      } catch {
        /* malformed message, ignore */
      }
    };
    source.onerror = () => {
      source?.close();
      dropped = true;
      // react-doctor-disable-next-line insecure-crypto-risk -- the random number spreads reconnects over a second so every window does not retry on the same tick; nothing here is a secret, a token or an id.
      if (!stopped) setTimeout(() => void connect(true), RETRY_MS + Math.random() * RETRY_JITTER_MS);
    };
  }
  void connect(false);

  return () => {
    stopped = true;
    source?.close();
  };
}
