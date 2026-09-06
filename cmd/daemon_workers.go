package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"ora/internal/brain"
	"ora/internal/config"
	"ora/internal/db"
	"ora/internal/embed"
	"ora/internal/tracker"
	"ora/internal/vector"
	"ora/internal/window"
)

// codexFallbackBrain holds the hand-over brain the unattended jobs use when Gemini answers 429 or 503. It is published once the daemon has built the ask agent, which happens after those jobs are wired, so it is read through an atomic rather than captured directly.
var codexFallbackBrain atomic.Pointer[brain.Brain]

// publishCodexFallback makes b the brain every unattended job hands over to on a quota or overload failure. Input: the Codex-backed brain; called once during startup.
func publishCodexFallback(b brain.Brain) {
	codexFallbackBrain.Store(&b)
}

// backgroundFallbackBrain returns the hand-over brain the unattended jobs should use, resolved at call time. Output: a Brain that answers through Codex once one has been published, and fails with a clear message before that.
func backgroundFallbackBrain() brain.Brain {
	return func(ctx context.Context, prompt string) (string, error) {
		b := codexFallbackBrain.Load()
		if b == nil {
			return "", fmt.Errorf("codex fallback is not wired yet")
		}
		return (*b)(ctx, prompt)
	}
}

// geminiRequestGate adapts brain.WithDailyQuota's Brain-shaped daily quota check into the Allow(model string) error shape memory.GeminiSummarizer's request gate expects, so its direct genai calls are metered against the same shared daily count as every Brain-wrapped call site above. The wrapped primary is a no-op: WithDailyQuota calls it only once its own quota check has already passed, so by the time it runs the count has already advanced and there is nothing left to do.
type geminiRequestGate struct {
	state *brain.QuotaState
	opts  brain.QuotaOptions
	// forAsks marks the interactive band, which may spend the share the quota keeps back from the nightly jobs.
	forAsks bool
}

// Allow reports whether a request against model may proceed in this gate's band: background like every unattended job, or interactive when forAsks is set.
func (g *geminiRequestGate) Allow(model string) error {
	noop := func(context.Context, string) (string, error) { return "", nil }
	_, err := brain.WithDailyQuota(g.state, model, g.forAsks, g.opts, noop)(context.Background(), "")
	return err
}

// reconcileEmbedCap bounds how many backfill embeds one ReconcileVectors sweep performs, to protect API quota on a large dirty store — the sweep runs again on the next trigger (startup / note consolidation) and picks up where it left off.
const reconcileEmbedCap = 200

// localReconcileEmbedCap is the same bound when the embedder is the local llama-server rather than a metered API. The cap exists to protect a quota; with a free embedder there is no quota to protect, and a small cap only means a backlog that never drains. Chunking made that backlog real — a store of 4,866 captures needs about 8,800 passage vectors, so at 200 a sweep it would take dozens of restarts to catch up.
const localReconcileEmbedCap = 5000

// reconcileCap picks the sweep's budget from whether embedding costs money.
func reconcileCap(embedsFree bool) int {
	if embedsFree {
		return localReconcileEmbedCap
	}
	return reconcileEmbedCap
}

// embedderAdapter adapts an embed.Embedder's Embed (which takes embed.TaskType) to the plain-string task param db.Store.SetEmbedder expects.
// internal/db can't import internal/embed, so the adapter lives here instead.
type embedderAdapter struct {
	inner embed.Embedder
}

func (e *embedderAdapter) Embed(ctx context.Context, task string, text string) ([]float32, error) {
	return e.inner.Embed(ctx, embed.TaskType(task), text)
}

// vectorIndexAdapter adapts *vector.ChromemIndex to db.Store.SetVectorIndex, translating vector.Result into db.Result field by field.
type vectorIndexAdapter struct {
	inner *vector.ChromemIndex
}

func (v *vectorIndexAdapter) Add(ctx context.Context, id, content string, embedding []float32, metadata map[string]string) error {
	return v.inner.Add(ctx, id, content, embedding, metadata)
}

func (v *vectorIndexAdapter) Search(ctx context.Context, queryEmbedding []float32, n int, where map[string]string) ([]db.Result, error) {
	results, err := v.inner.Search(ctx, queryEmbedding, n, where)
	if err != nil {
		return nil, err
	}
	out := make([]db.Result, len(results))
	for i, r := range results {
		out[i] = db.Result{
			ID:         r.ID,
			Content:    r.Content,
			Metadata:   r.Metadata,
			Similarity: r.Similarity,
		}
	}
	return out, nil
}

func (v *vectorIndexAdapter) Delete(ctx context.Context, id string) error {
	return v.inner.Delete(ctx, id)
}

func (v *vectorIndexAdapter) IDs() []string { return v.inner.IDs() }

// brainProviderName names a configured brain for the tally counters (see internal/tally.Wrap): the config's own provider constant when it's one of the recognised providers, "gemini" for the default/empty/explicit-Gemini-API case. Codex and Ollama belong here as much as the CLIs do — left out, a machine configured for either filed every counter it kept under gemini.
func brainProviderName(cfg config.BrainConfig) string {
	switch cfg.Provider {
	case config.BrainClaudeCLI, config.BrainAgyCLI, config.BrainGrokCLI, config.BrainCodex, config.BrainOllama:
		return cfg.Provider
	default:
		return "gemini"
	}
}

// drainEpisodes writes every activity the tracker publishes to the store and hands the same activity to the compiler, until events is closed. Input: the daemon's root context (its cancellation is dropped here), the tracker's channel, the store's episode writer, and the compiler's Ingest or nil when no compiler was built. Output: none — a failed write is logged and the next activity is still read.
// The cancellation is dropped because SIGTERM cancels the root context before stop() runs: every activity still in the channel, and every write already in flight, would otherwise fail with context canceled and the last minutes of a session would be lost. The context's values (the trace span) are kept.
func drainEpisodes(ctx context.Context, events <-chan tracker.Activity, write func(context.Context, db.EpisodeWrite) (int64, error), ingest func(context.Context, tracker.Activity)) {
	ctx = context.WithoutCancel(ctx)
	// The ingest runs on its own goroutine because it makes the attribution model call inline, with no deadline: run here, one stalled call stopped the drain, filled the tracker's event channel and ended all capture until a restart. One worker rather than one goroutine per activity keeps the buffer in the order the screens happened; an activity that finds the queue full is dropped from the summariser's buffer with a log line, having already been written to the store above.
	var pending chan tracker.Activity
	if ingest != nil {
		pending = make(chan tracker.Activity, ingestQueueDepth)
		defer close(pending)
		go func() {
			for ev := range pending {
				ingest(ctx, ev)
			}
		}()
	}
	for ev := range events {
		if _, err := write(ctx, db.EpisodeWrite{
			App: ev.App, Title: ev.Title, ScreenText: ev.ScreenText,
			UserActivity: ev.UserActivity, VisibleText: ev.VisibleText, ImageJPEG: ev.ImageJPEG,
			ExtraJPEG: ev.ExtraJPEG,
		}); err != nil {
			slog.Error("log episode failed", "error", err)
		}
		if pending != nil {
			select {
			case pending <- ev:
			default:
				slog.Warn("the activity compiler is behind, so this screen is not in the summary buffer", "app", ev.App)
			}
		}
	}
}

// ingestQueueDepth is how many activities may wait for the compiler before the drain starts dropping them. At the tracker's sampling rate this is roughly ten minutes of screens, which is longer than any attribution call that is going to come back at all.
const ingestQueueDepth = 256

// weeklyStudyMaterial finds what the Sunday distillation pass reads. Input: the data directory. Output: the replay transcripts and the dream traces, either of which may be empty. Replays are looked for in two places because the working directory is not the repo on every install: ora-restart pins it there, but the login autostart entry pins it to the binary's own directory and a packaged install has no evals/ at all, so <data>/replays is where a packaged install keeps them.
func weeklyStudyMaterial(dataDir string) (replays, traces []string) {
	repoReplays, _ := filepath.Glob("evals/replays/*.md")
	installedReplays, _ := filepath.Glob(filepath.Join(dataDir, "replays", "*.md"))
	traces, _ = filepath.Glob(filepath.Join(dataDir, "dreams", "*.jsonl"))
	return append(repoReplays, installedReplays...), traces
}

// jobFirstRunDelay is how long after startup a background job makes its first run, before its own interval takes over. Two minutes so the first run is not on the startup path, competing with the tracker and the embedding server for the machine.
const jobFirstRunDelay = 2 * time.Minute

// shutdownFlushBound is how long the shutdown waits for the compiler's last flush, and the deadline that flush's own context carries. Forty-five seconds because the flush makes an attribution model call and its writes: at the old ten the bound won on a normal call, and ForceFlush had already emptied the buffer, so that stretch of the session was lost.
const shutdownFlushBound = 45 * time.Second

// dreamArtifactRetention is how long a night's trace and its replay artifact are kept under <data>/dreams. Ninety days: the Sunday study reads the newest of them each week, and a night older than a quarter has already been distilled into lessons.md.
const dreamArtifactRetention = 90 * 24 * time.Hour

// ageDreamArtifacts removes the night traces and replay artifacts under <data>/dreams that were last written longer ago than keepFor. Input: the data directory and the retention. Output: how many files were removed, and the first error a removal returned.
// Only the two names the dream writes are matched, so anything else a person has put in that directory is left alone.
func ageDreamArtifacts(dataDir string, keepFor time.Duration) (int, error) {
	dir := filepath.Join(dataDir, "dreams")
	traces, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	replays, _ := filepath.Glob(filepath.Join(dir, "*-replay.md"))
	cutoff := time.Now().Add(-keepFor)
	removed := 0
	for _, path := range append(traces, replays...) {
		info, err := os.Stat(path)
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(path); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// every runs fn shortly after start and then on a ticker every interval until ctx is done — the ticker/select/ctx.Done skeleton every one of the daemon's background jobs otherwise repeated by hand. Each job's own logging/error-handling stays inside its fn closure; name is only for the stop-log line below.
func every(ctx context.Context, interval time.Duration, name string, fn func()) {
	everyAfter(ctx, jobFirstRunDelay, interval, name, fn)
}

// everyAfter is every with the first run's delay passed in, so a test does not wait out jobFirstRunDelay. Input: ctx, how long to wait before the first run, the interval between runs after that, the job's name for the stop-log line, and the job. Output: none — it returns when ctx is done.
// The first run exists because a job's interval is often longer than the machine's uptime: the image ageing runs every 24 h and the episodic compaction every 12 h, so on a machine restarted through the day neither ever fired and frames/ grew without bound.
func everyAfter(ctx context.Context, delay, interval time.Duration, name string, fn func()) {
	first := time.NewTimer(delay)
	defer first.Stop()
	select {
	case <-first.C:
		fn()
	case <-ctx.Done():
		slog.Debug("background job stopped", "job", name)
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			fn()
		case <-ctx.Done():
			slog.Debug("background job stopped", "job", name)
			return
		}
	}
}

// lateRaiser is the window raiser as the agent and the shutdown see it from the moment startup wires them, before the session-bus dial behind it has finished — or on a wedged bus, before it ever does. Every method answers "no window" until a Raiser is published, which is the same answer an uninstalled shell extension gives, so switch_window falls back to the keyboard exactly as it does on a machine that has none.
// It exists because the dial is bounded and its goroutine can outlive the bound: assigning the raiser from that goroutine wrote the agent's own field and the shutdown's variable while the ipc server was already answering asks on other goroutines. Publishing through an atomic and handing out this one stable value instead means nothing is written after startup but the pointer.
type lateRaiser struct {
	raiser atomic.Pointer[window.Raiser]
}

// publish makes r the raiser every later call goes to. Input: the dialled raiser. Output: none.
func (l *lateRaiser) publish(r *window.Raiser) { l.raiser.Store(r) }

// close releases the session-bus connection if one was ever dialled. Output: whatever Close returned, or nil when there is nothing to close.
func (l *lateRaiser) close() error {
	if r := l.raiser.Load(); r != nil {
		return r.Close()
	}
	return nil
}

func (l *lateRaiser) Available(ctx context.Context) (bool, error) {
	if r := l.raiser.Load(); r != nil {
		return r.Available(ctx)
	}
	return false, nil
}

func (l *lateRaiser) List(ctx context.Context) ([]window.Window, error) {
	if r := l.raiser.Load(); r != nil {
		return r.List(ctx)
	}
	return nil, nil
}

func (l *lateRaiser) ByPid(ctx context.Context, pid uint32) (bool, error) {
	if r := l.raiser.Load(); r != nil {
		return r.ByPid(ctx, pid)
	}
	return false, nil
}

func (l *lateRaiser) ByTitle(ctx context.Context, substring string) (bool, error) {
	if r := l.raiser.Load(); r != nil {
		return r.ByTitle(ctx, substring)
	}
	return false, nil
}

func (l *lateRaiser) ByWmClass(ctx context.Context, wmClass string) (bool, error) {
	if r := l.raiser.Load(); r != nil {
		return r.ByWmClass(ctx, wmClass)
	}
	return false, nil
}

// jobMarkerStore is the slice of the store the metered background jobs use to remember when they last ran. The diary table is where it goes because that is where every other once-per-period marker in the daemon already lives (see proactive.maybeWeeklyStudy); the row has no day, like the understanding doc, since a job's schedule is not a calendar day's.
type jobMarkerStore interface {
	DiaryEntry(ctx context.Context, day, kind string) (string, error)
	SetDiaryEntry(ctx context.Context, day, kind, content string) error
}

// jobMarkerKind is the diary kind one background job's last run is recorded under. Input: the job's name as it appears in the log lines. Output: the kind, prefixed so nothing else in the diary collides with it.
func jobMarkerKind(name string) string { return db.JobMarkerKindPrefix + name }

// lastJobRun reads when a background job last ran on this store. Input: ctx, the store, and the job's name. Output: the moment it last ran, or the zero time when it never has or the row could not be parsed.
func lastJobRun(ctx context.Context, store jobMarkerStore, name string) (time.Time, error) {
	raw, err := store.DiaryEntry(ctx, "", jobMarkerKind(name))
	if err != nil || raw == "" {
		return time.Time{}, err
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, nil
	}
	return at, nil
}

// firstRunDelay says how long a metered background job waits before its first run of this process. Input: when it last ran (zero when it never has), its interval, the shortest the first run may be away (jobFirstRunDelay in production), and now. Output: the later of that floor and the time left of the interval, so a restart resumes the schedule instead of starting it again.
func firstRunDelay(lastRun time.Time, interval, floor time.Duration, now time.Time) time.Duration {
	if lastRun.IsZero() {
		return floor
	}
	if left := lastRun.Add(interval).Sub(now); left > floor {
		return left
	}
	return floor
}

// everyMetered is every for a job that spends a model call on every run: same interval, but its last run is remembered in the store, so a restart does not buy another call two minutes in. Input: ctx, the store the marker row lives in, the interval, the job's name, and the job. Output: none — it returns when ctx is done.
// The plain first run exists because a 12 h or 24 h interval never fires on a machine restarted through the day, and that reasoning holds for the jobs that only read and write the store. For the ones that call the model it inverted the cost: on a day of ora-restart cycles the daemon paid for a flush attribution, a note consolidation and a compaction digest on every start.
func everyMetered(ctx context.Context, store jobMarkerStore, interval time.Duration, name string, fn func()) {
	everyMeteredAfter(ctx, store, jobFirstRunDelay, interval, name, fn)
}

// everyMeteredAfter is everyMetered with the first run's floor passed in, so a test does not wait out jobFirstRunDelay. Input: ctx, the store, the shortest the first run may be away, the interval, the job's name, and the job.
func everyMeteredAfter(ctx context.Context, store jobMarkerStore, floor, interval time.Duration, name string, fn func()) {
	delay := floor
	last, err := lastJobRun(ctx, store, name)
	if err != nil {
		slog.Warn("could not read a background job's last run, treating it as never run", "job", name, "error", err)
	} else {
		delay = firstRunDelay(last, interval, floor, time.Now())
	}
	everyAfter(ctx, delay, interval, name, func() {
		fn()
		if err := store.SetDiaryEntry(ctx, "", jobMarkerKind(name), time.Now().Format(time.RFC3339)); err != nil {
			slog.Warn("could not record a background job's last run", "job", name, "error", err)
		}
	})
}

// within runs one step and comes back at the bound whether or not the step finished, so a step that hangs delays the caller by that bound and no more. Input: the step's name for the log line, how long it may take, and the step itself. Output: none — a step still running when its bound passes is left behind and logged. At shutdown that is the right trade, since the process is about to exit and the alternative is a daemon that never releases its port; at startup it is the right trade because the port is already bound and nothing is accepting on it yet.
func within(name string, bound time.Duration, step func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		step()
	}()
	t := time.NewTimer(bound)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		slog.Warn("a bounded step did not finish in time and was left behind", "step", name, "bound", bound)
	}
}
