// Package components downloads, verifies and installs the optional local pieces June can use — whisper.cpp and its models, the sherpa-onnx diarizer, llama-server with the embedding and local text models — into the exact places the rest of the daemon already looks for them.
package components

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"june/internal/config"
)

// Options is what the downloader needs from the daemon. DataDir is June's data directory; ExeDir is the directory June's own executables are in (the installer puts the C runtime DLLs in its crt subdirectory); UpdateConfig changes the live config and saves it; Publish puts one progress event on the window's stream.
type Options struct {
	DataDir      string
	ExeDir       string
	UpdateConfig func(func(*config.JuneConfig)) error
	Publish      func(typ, id, text, detail string, failed bool)
}

// Service is the downloader. It runs one job at a time, in the order they were asked for: two downloads at once would only split the same line between them, and two installs at once could both be writing llama-server.
type Service struct {
	opts   Options
	man    *manifest
	client *http.Client

	// life ends when the daemon stops. Every job's context and every smoke test hangs off it, so Close stops all of them at once, while a cancel from the window ends one job's download and never a smoke test already under way.
	life    context.Context
	endLife context.CancelFunc

	platOnce sync.Once
	plat     Platform

	mu      sync.Mutex
	queue   []*job // queue[0] is running while working is true
	working bool
	// idle is closed when the worker goroutine running now returns, which is what Close waits on.
	idle chan struct{}
	// exclusive is set while a remove or the start-up sweep works on the same files the jobs do; the worker waits for it to end rather than starting alongside.
	exclusive bool
	closing   bool

	// stateMu guards state.json, which the job worker and the request handlers both write. Where both are taken, mu comes first.
	stateMu sync.Mutex
}

// job is one install the window or CI asked for: a feature, or a single component by its own id. replaces holds the parts of the variant being switched away from, which are deleted once the new one works, and replacesName that variant's name, which is what state.json keeps of it. after lists the prerequisites that were still queued when the job was asked for, which it checks again when its turn comes. starts and mark are pendingJob's. userCancelled tells a cancel from the window, which drops the job, from the daemon stopping, which keeps it for the next start.
type job struct {
	id            string
	feat          *feature
	variant       featureVariant
	parts         []part
	replaces      []part
	replacesName  string
	after         []string
	starts        int
	mark          int64
	ctx           context.Context
	cancel        context.CancelFunc
	running       bool
	userCancelled bool
}

// New makes the downloader and, on its own goroutine, takes up the jobs an earlier run of the daemon left queued or running. Only the daemon calls it, once it holds the IPC port, so no other process is working on the same queue.
func New(opts Options) *Service {
	s := newService(opts)
	if s.man != nil {
		go s.resume()
	}
	return s
}

// newService makes the downloader without touching its queue, for New and for june doctor's Status, which runs beside a live daemon.
func newService(opts Options) *Service {
	s := &Service{opts: opts, client: newHTTPClient()}
	s.life, s.endLife = context.WithCancel(context.Background())
	man, err := loadManifest()
	if err != nil {
		slog.Error("local features are unavailable", "error", err)
		return s
	}
	s.man = man
	return s
}

// maxStarts is how many daemon starts in a row may take up the same job without a clean stop, or any headway, between them. A download a quit, an update or a logoff cut short is taken up at the next start, and counts only when the run before got none of it onto the disk; a job that is still where it was after three starts that never got to stop cleanly is more likely what is bringing the daemon down than something that got interrupted.
const maxStarts = 3

// resume re-queues the jobs state.json says were queued or running when the daemon last stopped, in their order and with the builds they were asked with, so each download continues from its partial file. A job refused now — the feature was since set up by hand, say — is recorded as failed with the reason, the same as a refusal at install time.
func (s *Service) resume() {
	st := s.loadStateLocked()
	var jobs []*job
	var p Platform
	var recs map[string]receipt
	if len(st.Pending) > 0 {
		p, recs = s.platformCached(), s.receipts()
	}
	// The queue is empty until every job is resolved, so a feature queued behind its prerequisite — Who said what behind voice typing — would find it neither installed nor queued; the ids accepted so far stand in for the queue.
	coming := map[string]bool{}
	for i, pj := range st.Pending {
		j, ref := s.newJob(pj.ID, pj.Variant, p, recs, coming)
		if ref != nil {
			s.recordFailure(pj.ID, ref.message)
			continue
		}
		// Only the head of the list was running when the daemon went down; the jobs behind it were waiting, so they cannot be what brought it down. A head whose files grew since it was last taken up was cut short while getting somewhere (a shutdown or a logoff that never reached Close, which Windows ends after a few seconds), so its count starts again.
		switch {
		case i > 0:
			j.starts = pj.Starts
		case j.mark > pj.Mark:
			j.starts = 1
		default:
			j.starts = pj.Starts + 1
		}
		if j.starts > maxStarts {
			s.recordFailure(pj.ID, "the install was cut short several times in a row, so June stopped retrying it; try again")
			continue
		}
		coming[j.id] = true
		if pj.Replaces != "" && j.replacesName == "" && pj.Replaces != j.variant.Name && j.feat != nil {
			if _, old, ok := s.man.pick(j.feat, pj.Replaces, p); ok {
				j.replaces, j.replacesName = old, pj.Replaces
			}
		}
		jobs = append(jobs, j)
	}
	// The start-up sweep reclaims what the last run left — a staging directory, partials nobody came back for — and keeps the partials of the jobs about to resume.
	if s.beginExclusive() {
		s.sweep(jobPartials(jobs))
		s.endExclusive()
	}
	for _, j := range jobs {
		slog.Info("taking up a local-feature install the last run did not finish", "id", j.id, "variant", j.variant.Name, "start", j.starts)
	}
	if len(jobs) > 0 {
		s.enqueue(jobs...)
		return
	}
	if len(st.Pending) > 0 {
		// Every job was given up on, so the list they came from goes too.
		s.mu.Lock()
		s.savePendingLocked()
		s.mu.Unlock()
	}
}

// loadStateLocked reads state.json under the state lock.
func (s *Service) loadStateLocked() stateFile {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.loadState()
}

// recordFailure keeps a job's failure in state.json and tells the window, which shows it on the feature's card.
func (s *Service) recordFailure(id, msg string) {
	s.editState(func(st *stateFile) { st.Failed[id] = msg })
	s.publish(id, "failed", map[string]any{"error": msg}, true)
	slog.Warn("component install failed", "id", id, "error", msg)
}

// savePendingLocked writes the queue to state.json as the jobs the next start takes up. s.mu must be held, so the list written is the queue as it stands.
func (s *Service) savePendingLocked() {
	list := make([]pendingJob, 0, len(s.queue))
	for _, j := range s.queue {
		list = append(list, pendingJob{ID: j.id, Variant: j.variant.Name, Replaces: j.replacesName, Starts: j.starts, Mark: j.mark})
	}
	s.editState(func(st *stateFile) { st.Pending = list })
}

// Busy reports whether a download or install is queued or running, or a remove is under way, which a restart now would cut short.
func (s *Service) Busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue) > 0 || s.exclusive
}

// Close stops the downloader for the daemon's shutdown: no new job is taken, the running one is cancelled, and Close waits, until ctx ends, for it to stop. A component already being moved into place is finished first, as a cancel always lets it be. The queue stays in state.json, so the next start takes every job up again from its partial file.
func (s *Service) Close(ctx context.Context) {
	s.mu.Lock()
	s.closing = true
	s.endLife()
	idle := s.idle
	working := s.working
	s.mu.Unlock()
	if working {
		select {
		case <-idle:
		case <-ctx.Done():
			slog.Warn("a local-feature install did not stop in time for the shutdown")
			return
		}
	}
	// A clean stop: whatever is still queued was interrupted, not the cause of a crash, so its count of unclean starts begins again.
	s.mu.Lock()
	for _, j := range s.queue {
		j.starts = 0
		j.running = false
	}
	if len(s.queue) > 0 {
		s.savePendingLocked()
	}
	s.mu.Unlock()
}

// platformCached is the machine as first read, with free space, Smart App Control and whether the CUDA build failed on this card read fresh. The GPU and memory do not change under a running daemon, and reading the GPU runs nvidia-smi, so it is done once; Smart App Control can be turned on at any time since Windows' April 2026 update, and is one registry read. s.stateMu must not be held.
func (s *Service) platformCached() Platform {
	s.platOnce.Do(func() { s.plat = detectPlatform(s.opts.DataDir) })
	p := s.plat
	p.FreeBytes = freeBytes(s.opts.DataDir)
	p.SmartAppControl = smartAppControl()
	if g := p.GPU; g != nil {
		if m := s.loadStateLocked().NoCUDA; m != nil && m.GPU == g.Name && m.Driver == g.Driver {
			p.noCUDA = true
		}
	}
	return p
}

// publish puts one "component" event on the window's stream. detail is marshalled to JSON, which is the shape the window parses.
func (s *Service) publish(id, text string, detail any, failed bool) {
	if s.opts.Publish == nil {
		return
	}
	d := ""
	if detail != nil {
		if b, err := json.Marshal(detail); err == nil {
			d = string(b)
		}
	}
	s.opts.Publish("component", id, text, d, failed)
}

// publishStep announces a step of a job that has no byte count of its own.
func (s *Service) publishStep(prog *progress, text, file string) {
	s.publish(prog.id, text, map[string]any{"file": file}, false)
}

// jobState is "installing" or "queued" for an id with a job in the queue, and "" otherwise.
func (s *Service) jobState(id string) string {
	st, _, _ := s.jobFor(id)
	return st
}

// jobFor is jobState with the build the job is installing, read under s.mu since a fall back from CUDA changes it mid-job.
func (s *Service) jobFor(id string) (string, featureVariant, []part) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.queue {
		if j.id != id {
			continue
		}
		if j.running {
			return "installing", j.variant, j.parts
		}
		return "queued", j.variant, j.parts
	}
	return "", featureVariant{}, nil
}

// errClosing is an install asked for while the daemon is shutting down.
var errClosing = errors.New("June is shutting down; try again once it is back")

// enqueue adds jobs, skipping any whose id is already queued or running, clears the failures on record for the ones added (a new attempt is under way, so a cancel of it must not bring the old error back), writes the queue to state.json and starts the worker if it is idle. Output: errClosing, with nothing added, once Close has been called.
func (s *Service) enqueue(jobs ...*job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return errClosing
	}
	var added []string
	for _, j := range jobs {
		if s.queuedLocked(j.id) {
			continue
		}
		j.ctx, j.cancel = context.WithCancel(s.life)
		s.queue = append(s.queue, j)
		added = append(added, j.id)
	}
	if len(added) == 0 {
		return nil
	}
	s.editState(func(st *stateFile) {
		for _, id := range added {
			delete(st.Failed, id)
		}
	})
	s.savePendingLocked()
	s.startWorkerLocked()
	return nil
}

func (s *Service) queuedLocked(id string) bool {
	for _, q := range s.queue {
		if q.id == id {
			return true
		}
	}
	return false
}

// startWorkerLocked starts the worker when there is work, none is running, and nothing holds the files exclusively. s.mu must be held.
func (s *Service) startWorkerLocked() {
	if s.working || s.exclusive || s.closing || len(s.queue) == 0 {
		return
	}
	s.working = true
	s.idle = make(chan struct{})
	go s.work(s.idle)
}

// beginExclusive claims the component files for a remove or the start-up sweep, which delete what a job may be writing. Output: false while a job is queued or running, another claim is held, or the daemon is closing; nothing is claimed then.
func (s *Service) beginExclusive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) > 0 || s.exclusive || s.closing {
		return false
	}
	s.exclusive = true
	return true
}

// endExclusive gives the claim back and starts any job asked for while it was held.
func (s *Service) endExclusive() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exclusive = false
	s.startWorkerLocked()
}

// work runs queued jobs until there are none or the daemon is closing. A job the shutdown interrupted stays at the head of the queue and in state.json, for the next start to take up. Each job's outcome reaches the window as events and its failure is recorded in state.json, so nothing is returned. Input: the channel to close on return, which Close waits on.
func (s *Service) work(idle chan struct{}) {
	defer close(idle)
	for {
		s.mu.Lock()
		if len(s.queue) == 0 || s.closing {
			s.working = false
			s.mu.Unlock()
			return
		}
		j := s.queue[0]
		j.running = true
		keep := jobPartials(s.queue)
		s.mu.Unlock()

		finished := s.run(j, keep)

		s.mu.Lock()
		if finished {
			s.queue = s.queue[1:]
			j.cancel()
			s.savePendingLocked()
		}
		s.mu.Unlock()
	}
}

// cancelJob stops the job for id: a queued one is dropped, a running one is told to stop and reports "cancelled" itself once it has. Output: false when there is no such job.
func (s *Service) cancelJob(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, j := range s.queue {
		if j.id != id {
			continue
		}
		if j.running {
			j.userCancelled = true
			j.cancel()
			return true
		}
		s.queue = append(s.queue[:i:i], s.queue[i+1:]...)
		j.cancel()
		s.savePendingLocked()
		go s.publish(id, "cancelled", nil, false)
		return true
	}
	return false
}

// cancelledByUser reports whether the window cancelled the job, as against the daemon stopping under it.
func (s *Service) cancelledByUser(j *job) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return j.userCancelled
}

// progress is one job's download progress across every file it fetches, so the window can show one bar per feature. done holds the bytes on disk of each artifact, which a resumed download starts from rather than from zero.
type progress struct {
	s     *Service
	id    string
	total int64
	done  map[*artifact]int64
	sum   int64
	file  string

	lastAt  time.Time
	lastSum int64
	bps     float64
}

func newProgress(s *Service, id string, total int64) *progress {
	return &progress{s: s, id: id, total: total, done: map[*artifact]int64{}}
}

// resumeAt says an artifact's download starts from n bytes already on disk. The jump is not counted as speed.
func (p *progress) resumeAt(a *artifact, n int64) {
	p.sum += n - p.done[a]
	p.done[a] = n
	p.lastSum = p.sum
	p.file = a.fileName()
	p.emit(true)
}

// add counts n more bytes of an artifact.
func (p *progress) add(a *artifact, n int64) {
	p.done[a] += n
	p.sum += n
	p.emit(false)
}

// emit publishes a "downloading" event, no more often than progressEvery unless forced. bps is smoothed, so a burst after a stall does not read as the line's real speed.
func (p *progress) emit(force bool) {
	now := time.Now()
	elapsed := now.Sub(p.lastAt)
	if !force && elapsed < progressEvery {
		return
	}
	if !p.lastAt.IsZero() && elapsed > 0 && p.sum >= p.lastSum {
		rate := float64(p.sum-p.lastSum) / elapsed.Seconds()
		if p.bps == 0 {
			p.bps = rate
		} else {
			p.bps = 0.7*p.bps + 0.3*rate
		}
	}
	p.lastAt, p.lastSum = now, p.sum
	p.s.publish(p.id, "downloading", map[string]any{"file": p.file, "done": p.sum, "total": p.total, "bps": int64(p.bps)}, false)
}
