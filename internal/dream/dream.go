// Package dream is the daemon's overnight loop: while the machine sits idle on mains between the dream hour and the morning brief, it tests the hypotheses Ora's diary has accumulated against the week's evidence, adopts new ones, rewrites the standing understanding of the user, and leaves a morning report in the diary. This slice is judge-only — every model call goes to the configured brain; the local grinder model is a later slice.
package dream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"ora/internal/brain"
	"ora/internal/db"
)

// dayFormat is the local calendar-day key shared with the diary and dream_runs tables.
const dayFormat = "2006-01-02"

const (
	// brainCurfewMinutes is the local time-of-day, in minutes after midnight, past which no unforced dream may start a brain call: the user's Claude subscription runs in five-hour windows and his workday window opens at 08:30, so any call after 03:28 would open an overnight window that bleeds into it. A forced run (the dream-now marker) bypasses the curfew — forcing is the user's own choice.
	brainCurfewMinutes = 3*60 + 25
	// idleAfter is how long the newest episode must be old for the user to count as away when the screen is not locked.
	idleAfter = 15 * time.Minute
	// missingDiaryGrace is how long past the dream hour the runner waits for the evening close's diary entry before dreaming from raw day summaries instead.
	missingDiaryGrace = 2 * time.Hour
	// maxNewHypotheses caps how many hypotheses one night may adopt from the diary.
	maxNewHypotheses = 5
	// maxStatementLen caps an adopted hypothesis statement; longer ones are dropped as malformed rather than truncated mid-sentence.
	maxStatementLen = 200
	// maxEvidenceLen caps a verdict's evidence citation; longer ones are truncated.
	maxEvidenceLen = 500
	// promoteAfterTests and promoteMinAgeDays are the promotion mechanics: the judge may recommend, but only a hypothesis tested this many times and born at least this many days ago is promoted.
	promoteAfterTests = 3
	promoteMinAgeDays = 7
	// retireContradictions retires a hypothesis once the evidence log holds this many contradictions.
	retireContradictions = 2
	// staleAfterDays retires an open hypothesis that has sat this long without ever being tested.
	staleAfterDays = 30
	// defaultWatchEvery is how often the preemption watcher polls for the user's return.
	defaultWatchEvery = 5 * time.Second
	// inputFreshAfter is how fresh InputIdle's report must be for the watcher to read it as "real input just happened". Below the polling cadence of a moment ago, so a probe read moments after a keypress still counts.
	inputFreshAfter = 30 * time.Second
	// evidenceBudget caps the assembled evidence material in bytes; when the week holds more, the oldest items fall away first so the judge always reads the newest material.
	evidenceBudget = 24 * 1024
	// evidenceThreads is how many active threads the evidence lists.
	evidenceThreads = 30
	// compactAfterDays is how old every daily in a Mon-Sun week must be before the week collapses into one kind='week' diary entry.
	compactAfterDays = 7
	// compactWeeksToMonth is how old, in weeks, a week entry must be before it may fold into its month's entry.
	compactWeeksToMonth = 10
)

// contradictedMark is what a contradiction's evidence line contains, and what the retirement mechanic counts. The lines are written by evidenceLine below, so the format is ours to rely on.
const contradictedMark = "] contradicted —"

// errUnparsable marks a brain reply that failed to parse as JSON even after one re-ask, so the caller skips the work instead of retrying it all night.
var errUnparsable = errors.New("the reply never parsed as JSON")

// Probes are the runner's yes/no reads on the machine — mains power, the lock screen, real input idle time, and whether the meeting recorder is mid-flight — injected so tests can flip them freely.
type Probes struct {
	OnAC              func() bool
	SessionLocked     func() bool
	RecorderQuiescent func() bool
	// InputIdle reports real time since the last keyboard/mouse input (GNOME's Mutter IdleMonitor). Optional: nil keeps the old newest-episode heuristic as the away signal, since a screen-content change (a title bar updating on autoplay or an unread count) is not evidence the user touched anything.
	InputIdle func() (time.Duration, error)
}

// Store is the slice of *db.Store the dreaming needs: the night's own run bookkeeping and hypothesis ledger, the diary it reads and rewrites, and the grounded evidence every stage judges against. Declared here rather than taking *db.Store whole, so this package states its entire data dependency in one place and widening it is a deliberate edit instead of an accident.
type Store interface {
	// The night's run row and its stage tokens. A wake that finds an unfinished run resumes from stages_done rather than repeating committed work.
	StartDreamRun(ctx context.Context, night string) error
	DreamRun(ctx context.Context, night string) (run db.DreamRun, ok bool, err error)
	FinishDreamRun(ctx context.Context, night, entry, line string) error
	CommitHypothesisStage(ctx context.Context, night string, verdicts []db.HypothesisVerdict, adopted []db.NewHypothesis) error
	CommitUnderstandingStage(ctx context.Context, night, understanding string) error
	CommitCompactStage(ctx context.Context, night string, comps []db.DiaryCompaction, done bool) error
	CommitReplayStage(ctx context.Context, night string) error
	CommitProceduresStage(ctx context.Context, night string) error
	CommitPruneStage(ctx context.Context, night string) error

	// The hypothesis ledger the night tests against the week's evidence and adds to.
	OpenHypotheses(ctx context.Context) ([]db.Hypothesis, error)
	StrongHypotheses(ctx context.Context) ([]db.Hypothesis, error)

	// The diary. The evening close's entry is the night's starting material, and the compaction stage rewrites older entries in place.
	DiaryEntry(ctx context.Context, day, kind string) (string, error)
	DiaryDays(ctx context.Context, from, to string) ([]db.DiaryDay, error)
	DiaryEntriesThrough(ctx context.Context, kind, through string) ([]db.DiaryDay, error)

	// The screen-tool runs the procedures stage reads back, and the note path it writes each goal's procedure through.
	ActRuns(ctx context.Context, limit int) ([]db.ActRun, error)
	LogNote(ctx context.Context, content, kind string) (int64, error)

	// CloseDoneActionItems closes the open tasks that a meeting, a day's page or a compiled note says are finished, and reports how many moved.
	CloseDoneActionItems(ctx context.Context, since time.Time) (int, error)

	// The two retention passes the pruning stage runs, and the counts of what each one's policy held back, so the night can log what it kept as well as what it took.
	ProtectedConversations(ctx context.Context, olderThan time.Duration) (int64, error)
	PruneEmptyConversations(ctx context.Context, olderThan time.Duration) (int64, error)
	ProtectedActRuns(ctx context.Context, failedGrace time.Duration) (withNotes, failedYoung int64, err error)
	PruneActRuns(ctx context.Context, keep int, failedGrace time.Duration) (int64, error)

	// The grounded evidence the judging calls read, plus the store's own notion of "now" so a night is not measured against the wrong day.
	SummaryTimeline(ctx context.Context, since, until time.Time) ([]db.WindowSummary, error)
	ActiveThreads(ctx context.Context, limit int) ([]db.Thread, error)
	NotesOfKindSince(ctx context.Context, kind string, since time.Time) ([]db.Note, error)
	MemoryAsOf(ctx context.Context, source string) (time.Time, error)
}

// Runner owns one machine's dreaming. Construct with New; the daemon calls Tick on a ticker and everything else is private.
type Runner struct {
	// OnNight, when set, is told when a night's run begins (true) and when it ends however it ends (false), so a window can show that Ora is dreaming.
	OnNight func(running bool)
	store   Store
	brain   brain.Brain
	probes  Probes
	// dreamHour opens the window (negative disables dreaming); briefHour closes it. Both local hours.
	dreamHour int
	briefHour int
	// now is the clock, replaceable in tests. All night-key and window math is local time.
	now func() time.Time
	// watchEvery paces the preemption watcher. Tests shorten it.
	watchEvery time.Duration
	// retention reports the two act run retention numbers the pruning stage obeys — the count cap and the failed-run grace — read from the config file rather than chosen here, so the policy is editable. Replaceable in tests, like now above.
	retention func() (keep int, failedGrace time.Duration)

	// ForceMarker is the path of a file whose presence makes the next tick dream immediately, bypassing the window and away-gates — the way to watch a dream run without leaving the machine. The marker is consumed, and a forced run arms no preemption watcher, since the user being present is the whole point.
	ForceMarker string

	// CurfewExempt lifts the Claude-window curfew: a dream brain that is not claude-cli (grok, agy) spends no Claude usage, so it may run right up to the morning brief.
	CurfewExempt bool

	// DataDir, when set, is where night traces land: every brain call's kind and raw reply appended to <DataDir>/dreams/<night>.jsonl as distillation raw material for a later weekly study pass. Empty disables tracing.
	DataDir string

	// Shadow is an optional second brain fired with the same prompt as every primary brain call, purely for offline comparison: its reply is traced under the primary call's kind with "-shadow" appended and never parsed, stored, or allowed to affect a verdict. Nil disables shadowing.
	Shadow brain.Brain

	// ShadowLifecycle optionally starts the server Shadow talks to before the night's stages and stops it after. Zero value (both funcs nil) means the shadow's backend is already reachable, or there is none to manage.
	ShadowLifecycle ShadowLifecycle

	// activeShadow is what ask() actually fires this run: a copy of Shadow, cleared for the run alone when ShadowLifecycle.Start fails, so a bad night never mutates the Shadow the next night would otherwise get.
	activeShadow brain.Brain

	// GPUReleaser asks the GPU's other tenant to leave before the shadow lifecycle starts — the daemon points it at the embedding server's StopIfIdle. Best-effort and nil-safe: its bool return is ignored, since a shadow that fails to start already runs the night shadowless. Nil means there is nothing sharing the card.
	GPUReleaser func() bool
}

// New builds a Runner from the store, a one-shot brain, the machine probes, and the two local hours that bound the window.
func New(store Store, b brain.Brain, probes Probes, dreamHour, briefHour int) *Runner {
	return &Runner{store: store, brain: b, probes: probes, dreamHour: dreamHour, briefHour: briefHour, now: time.Now, watchEvery: defaultWatchEvery, retention: configRetention}
}

// nightKey returns the night a moment belongs to: the current local day once the dream hour has passed, otherwise the day before — so 23:30 and 02:00 the next morning are the same night.
// closingEvidenceWindow is how far back the nightly sweep reads for writing that says a task is finished. A week, so a task closed in a meeting the daemon was down for still gets picked up, without re-reading every meeting ever recorded each night.
const closingEvidenceWindow = 7 * 24 * time.Hour

func (r *Runner) nightKey(now time.Time) string {
	if now.Hour() >= r.dreamHour {
		return now.Format(dayFormat)
	}
	return now.AddDate(0, 0, -1).Format(dayFormat)
}

// inWindow reports whether the hour lies in [dreamHour, briefHour), handling the usual case where the window wraps midnight.
func (r *Runner) inWindow(hour int) bool {
	if r.dreamHour < r.briefHour {
		return hour >= r.dreamHour && hour < r.briefHour
	}
	return hour >= r.dreamHour || hour < r.briefHour
}

// windowStart is the local instant the night's window opened, for the missing-diary grace check.
func (r *Runner) windowStart(night string) time.Time {
	d, err := time.ParseInLocation(dayFormat, night, time.Local)
	if err != nil {
		return time.Time{}
	}
	return time.Date(d.Year(), d.Month(), d.Day(), r.dreamHour, 0, 0, 0, time.Local)
}

// nightMinus returns the night key days earlier, for age comparisons — ISO date strings compare lexically.
func nightMinus(night string, days int) string {
	d, err := time.ParseInLocation(dayFormat, night, time.Local)
	if err != nil {
		return night
	}
	return d.AddDate(0, 0, -days).Format(dayFormat)
}

// userAway reports whether the user counts as away, given the session is not locked: real input idle time (GetIdletime) when the probe is wired and healthy, since that is actual keyboard/mouse activity — a screen-content change (autoplay rolling to the next episode, an unread-count title) is not. When the probe is nil or errors, this falls back to the old heuristic: no episode ever, or the newest one older than idleAfter.
func (r *Runner) userAway(now, lastEpisode time.Time) bool {
	if r.probes.InputIdle != nil {
		if idle, err := r.probes.InputIdle(); err == nil {
			return idle >= idleAfter
		}
	}
	return lastEpisode.IsZero() || now.Sub(lastEpisode) >= idleAfter
}

// Tick is the per-wake entry point, called from the daemon's five-minute ticker. It returns without a trace when the window is closed or any start condition fails, and otherwise starts or resumes the night's run. Conditions, all required: inside the window, the night not yet finished, on mains, the user idle (screen locked, or away per userAway), the recorder quiescent, and the evening close's diary entry present — or missingDiaryGrace past the dream hour, in which case day summaries stand in.
func (r *Runner) Tick(ctx context.Context) {
	if r.dreamHour < 0 {
		return
	}
	now := r.now().In(time.Local)
	forced := r.consumeForceMarker()
	if !forced && !r.inWindow(now.Hour()) {
		return
	}
	// Past the curfew the whole run waits for tomorrow night: a dream started at 03:20 could still be making brain calls at 03:40, so the gate is on starting at all, with a resumed run equally held. Only Claude spends the user's usage windows, so a non-claude dream brain is exempt.
	if !forced && !r.CurfewExempt && now.Hour()*60+now.Minute() >= brainCurfewMinutes && now.Hour() < r.briefHour {
		return
	}
	night := r.nightKey(now)
	run, exists, err := r.store.DreamRun(ctx, night)
	if err != nil {
		slog.Warn("dreaming: reading the run row failed", "night", night, "error", err)
		return
	}
	if exists && run.Finished {
		return
	}
	locked := r.probes.SessionLocked()
	lastEpisode, err := r.store.MemoryAsOf(ctx, "episode:recent")
	if err != nil {
		slog.Warn("dreaming: reading the newest episode failed", "error", err)
		return
	}
	if !forced {
		if !r.probes.OnAC() {
			return
		}
		if !locked && !r.userAway(now, lastEpisode) {
			return
		}
		if !r.probes.RecorderQuiescent() {
			return
		}
	}
	dayEntry, err := r.store.DiaryEntry(ctx, night, "day")
	if err != nil {
		slog.Warn("dreaming: reading the night's diary entry failed", "error", err)
		return
	}
	fallback := dayEntry == ""
	if !forced && fallback && now.Sub(r.windowStart(night)) < missingDiaryGrace {
		return
	}
	r.dream(ctx, night, run, exists, locked && !forced, lastEpisode, fallback, forced)
}

// SetBrain swaps which backend dreams — the daemon calls it when the config names a dedicated dream brain.
func (r *Runner) SetBrain(b brain.Brain) { r.brain = b }

// consumeForceMarker reports whether the manual dream trigger is set, removing it so one touch means one run.
func (r *Runner) consumeForceMarker() bool {
	if r.ForceMarker == "" {
		return false
	}
	if _, err := os.Stat(r.ForceMarker); err != nil {
		return false
	}
	if err := os.Remove(r.ForceMarker); err != nil {
		slog.Warn("dreaming: could not consume the force marker", "path", r.ForceMarker, "error", err)
		return false
	}
	slog.Info("dreaming: manual trigger, running now with the away-gates bypassed")
	return true
}

// dream runs (or resumes) one night: start the run row, arm the preemption watcher, run the missing stages, and finish with the morning report. Any stage error — a cancelled context included — just returns; nothing partial was committed and the next wake resumes from stages_done.
func (r *Runner) dream(ctx context.Context, night string, run db.DreamRun, exists, lockedAtStart bool, baseline time.Time, fallback, forced bool) {
	if r.OnNight != nil {
		r.OnNight(true)
		defer r.OnNight(false)
	}
	if !exists {
		if err := r.store.StartDreamRun(ctx, night); err != nil {
			slog.Warn("dreaming: starting the run failed", "night", night, "error", err)
			return
		}
		slog.Info("dreaming: starting the night", "night", night, "mode", "judge-only")
	} else {
		slog.Info("dreaming: resuming the night", "night", night, "stages_done", run.StagesDone)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if !forced {
		go r.watchForUser(ctx, cancel, lockedAtStart, baseline)
	}

	r.activeShadow = r.Shadow
	if r.activeShadow != nil && r.ShadowLifecycle.Start != nil {
		if r.GPUReleaser != nil {
			r.GPUReleaser()
		}
		if err := r.ShadowLifecycle.Start(ctx); err != nil {
			slog.Warn("dreaming: shadow lifecycle failed to start, running the night without a shadow", "night", night, "error", err)
			r.activeShadow = nil
		} else if r.ShadowLifecycle.Stop != nil {
			defer r.ShadowLifecycle.Stop()
		}
	}

	started := r.now()
	done := strings.Fields(run.StagesDone)
	var notes []string
	if fallback {
		notes = append(notes, "Tonight's diary entry was missing, so the day's screen summaries stood in for it.")
	}

	var hyp *stageReport
	if !slices.Contains(done, "hyp") {
		rep, err := r.hypStage(ctx, night, fallback)
		if err != nil {
			slog.Warn("dreaming: hypothesis stage did not commit", "night", night, "error", err)
			return
		}
		hyp = &rep
		slog.Info("dreaming: hypothesis stage committed", "night", night, "tested", rep.tested, "promoted", rep.promoted, "retired", rep.retired, "adopted", rep.adopted)
	}

	undRan := false
	if !slices.Contains(done, "und") {
		if err := r.undStage(ctx, night); err != nil {
			slog.Warn("dreaming: understanding stage did not commit", "night", night, "error", err)
			return
		}
		undRan = true
		slog.Info("dreaming: understanding stage committed", "night", night)
	}

	var comp *compactReport
	if !slices.Contains(done, "compact") {
		rep, err := r.compactStage(ctx, night)
		if err != nil {
			slog.Warn("dreaming: compaction stage did not commit", "night", night, "error", err)
			return
		}
		comp = &rep
		slog.Info("dreaming: compaction stage committed", "night", night, "weeks", rep.weeks, "months", rep.months)
	}

	var replay *replayReport
	if !slices.Contains(done, "replay") {
		rep, err := r.replayStage(ctx, night)
		if err != nil {
			slog.Warn("dreaming: replay stage did not commit", "night", night, "error", err)
			return
		}
		replay = &rep
		slog.Info("dreaming: replay stage finished", "night", night, "skipped", rep.skipped, "partial", rep.partial, "items", rep.items, "piles", rep.piles)
	}

	// The procedures stage now carries a stages_done token like the other stages, so a night that already ran it does not re-read the act runs on a later wake. Its error handling stays soft, though: a stage failure (or a failed commit) is only logged and the night carries on — a missing procedure note must not cost the night its morning report the way a failed hypothesis or understanding stage does.
	if !slices.Contains(done, "procedures") {
		if proc, err := r.proceduresStage(ctx, night); err != nil {
			slog.Warn("dreaming: procedures stage failed", "night", night, "error", err)
		} else {
			slog.Info("dreaming: procedures stage finished", "night", night, "runs", proc.runs, "goals", proc.goals, "written", proc.written, "skipped", proc.skipped)
			if proc.written > 0 {
				notes = append(notes, procedureLine(proc))
			}
			if err := r.store.CommitProceduresStage(ctx, night); err != nil {
				slog.Warn("dreaming: procedures stage did not commit its token", "night", night, "error", err)
			}
		}
	}

	// The tasks the day's meetings and pages say are finished are closed before the night's report, so the morning brief is not read out a list of work that was already done. Soft like the stages below it and carrying no token: the sweep only ever closes what evidence names, so running it twice closes nothing twice.
	if closed, err := r.store.CloseDoneActionItems(ctx, r.now().Add(-closingEvidenceWindow)); err != nil {
		slog.Warn("dreaming: could not close the tasks the week's writing says are finished", "night", night, "error", err)
	} else if closed > 0 {
		slog.Info("dreaming: closed tasks the week's writing says are finished", "night", night, "closed", closed)
	}

	// The pruning stage goes last, after the procedures stage has taken what it wanted from the act runs: a run only becomes safe from the count cap once the note written from it exists. Its error handling is the procedures stage's — a store that could not be reached is logged, the token is left off so the next wake retries, and the night still gets its morning report.
	if !slices.Contains(done, "prune") {
		if p, err := r.pruneStage(ctx); err != nil {
			slog.Warn("dreaming: pruning stage failed, nothing was pruned tonight", "night", night, "error", err)
		} else {
			slog.Info("dreaming: pruning stage finished", "night", night,
				"conversations_removed", p.conversationsRemoved, "conversations_kept", p.conversationsProtected,
				"act_runs_removed", p.runsRemoved, "act_runs_kept_for_notes", p.runsKeptForNotes, "act_runs_kept_failed", p.runsKeptFailedYoung)
			if err := r.store.CommitPruneStage(ctx, night); err != nil {
				slog.Warn("dreaming: pruning stage did not commit its token", "night", night, "error", err)
			}
		}
	}

	if err := r.finish(ctx, night, r.now().Sub(started), hyp, undRan, comp, replay, notes); err != nil {
		slog.Warn("dreaming: finishing the night failed", "night", night, "error", err)
		return
	}
	slog.Info("dreaming: night finished", "night", night)
}

// watchForUser cancels the night's work the moment the user comes back: the session unlocking (when the lock was the idle signal at start), or real input arriving fresh per InputIdle when that probe is wired and healthy. Only when the probe is nil or erroring does a new episode fall back as the return signal — screen-content changes (autoplay, an unread-count title) are not activity, so they must not preempt a night the input probe still calls idle. A cancelled stage's transaction never commits, so preemption loses at most one in-flight brain call.
func (r *Runner) watchForUser(ctx context.Context, cancel context.CancelFunc, lockedAtStart bool, baseline time.Time) {
	t := time.NewTicker(r.watchEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if lockedAtStart && !r.probes.SessionLocked() {
				slog.Info("dreaming preempted: the session unlocked")
				cancel()
				return
			}
			if r.probes.InputIdle != nil {
				if idle, err := r.probes.InputIdle(); err == nil {
					if idle < inputFreshAfter {
						slog.Info("dreaming preempted: real input arrived")
						cancel()
						return
					}
					continue
				}
			}
			if last, err := r.store.MemoryAsOf(ctx, "episode:recent"); err == nil && last.After(baseline) {
				slog.Info("dreaming preempted: a new episode arrived")
				cancel()
				return
			}
		}
	}
}

// ask runs one traced brain call: the raw reply — any thinking text a model emits included — lands in the night's trace file before anything parses it. When a shadow is active, the same prompt is then fired at it too, after the primary call has already returned — its reply is only ever traced, never used for anything the primary call's result feeds.
func (r *Runner) ask(ctx context.Context, night, kind, prompt string) (string, error) {
	reply, err := r.brain(ctx, prompt)
	r.traceCall(night, kind, reply, err)
	if r.activeShadow != nil {
		r.shadowAsk(ctx, night, kind, prompt)
	}
	return reply, err
}

// shadowAsk fires prompt at the active shadow brain under its own generous timeout (a local Q2 model can take minutes on a long prompt), tracing the reply under kind+"-shadow" into the same night's JSONL. It still respects the parent ctx: a preempted night (the user came back) cuts the shadow call short exactly like the primary one. A shadow failure is only logged — it never fails the stage that called ask, since the shadow never influences the night.
func (r *Runner) shadowAsk(ctx context.Context, night, kind, prompt string) {
	shadowCtx, cancel := context.WithTimeout(ctx, shadowTimeout)
	defer cancel()
	reply, err := r.activeShadow(shadowCtx, prompt)
	r.traceCall(night, kind+"-shadow", reply, err)
	if err != nil {
		slog.Warn("dreaming: shadow brain call failed", "night", night, "kind", kind, "error", err)
	}
}

// traceCall appends one JSONL line for a brain call to <DataDir>/dreams/<night>.jsonl. Best-effort by design: a failed trace write is logged and never fails a stage.
func (r *Runner) traceCall(night, kind, reply string, callErr error) {
	if r.DataDir == "" {
		return
	}
	rec := struct {
		At    string `json:"at"`
		Kind  string `json:"kind"`
		Reply string `json:"reply,omitempty"`
		Error string `json:"error,omitempty"`
	}{At: r.now().Format(time.RFC3339), Kind: kind, Reply: reply}
	if callErr != nil {
		rec.Error = callErr.Error()
	}
	line, err := json.Marshal(rec)
	if err != nil {
		slog.Warn("dreaming: could not marshal a night trace", "error", err)
		return
	}
	dir := filepath.Join(r.DataDir, "dreams")
	// The trace holds raw brain replies — diary text, screen summaries, people's names — so it gets the same 0600 in a 0700 directory the frames and the database already use, rather than relying on the data dir's own mode to cover it.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		slog.Warn("dreaming: could not create the traces dir", "dir", dir, "error", err)
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, night+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		slog.Warn("dreaming: could not open the night's trace file", "night", night, "error", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		slog.Warn("dreaming: could not write a night trace", "night", night, "error", err)
	}
}

// askJSON runs one traced brain call and decodes its JSON reply into out, re-asking the same prompt once when the reply fails to parse. A transport error returns as-is so the stage can retry on a later wake; a reply that never parses returns errUnparsable so the caller skips instead.
func (r *Runner) askJSON(ctx context.Context, night, kind, prompt string, out any) error {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		reply, err := r.ask(ctx, night, kind, prompt)
		if err != nil {
			return err
		}
		body := brain.StripFence(reply)
		if err := json.Unmarshal([]byte(body), out); err == nil {
			return nil
		} else if sliced := brain.OutermostJSON(body); sliced != "" && json.Unmarshal([]byte(sliced), out) == nil {
			// Some models pad the array with prose no instruction talks them out of; the payload between the outermost brackets is still exactly what was asked for.
			return nil
		} else {
			lastErr = err
			slog.Warn("dreaming: a brain reply failed to parse as JSON", "attempt", attempt+1, "error", err)
		}
	}
	return fmt.Errorf("%w: %v", errUnparsable, lastErr)
}
