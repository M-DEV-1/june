package actjob

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// wallTick is how often the wall-clock guard looks at how long a job has been working: fine enough grain against a five-minute budget, and one wakeup every tenth of a second while a job runs.
const wallTick = 100 * time.Millisecond

// live is one job actually running: its state under a lock, the context and cancel that stop it, and the two channels the user's answers and resumes arrive on.
type live struct {
	mu      sync.Mutex
	job     Job
	ctx     context.Context
	cancel  context.CancelFunc
	answers chan string
	resumes chan struct{}
	paused  atomic.Bool
	stopped atomic.Bool

	// started is when this run of the job began and baseMS is what earlier runs of it had already spent, so the wall budget is one budget across a restart rather than a fresh one each time.
	started time.Time
	baseMS  int64
	// waitedMS is how long this run has already spent waiting on the user, and waitingSince is when the wait it is in now began, zero when it is not waiting. Neither counts against the wall budget: a job holding still for a person is not a job running away with the screen.
	waitedMS     atomic.Int64
	waitingSince atomic.Int64
	// working is how endWait tells the wall-clock guard that the job has gone back to work, so the guard can sleep through a wait instead of polling a frozen clock. It holds one token, and a token left over from a wait the guard never saw costs one extra wakeup and nothing else.
	working chan struct{}
}

// snapshot gives a caller a safe copy of a live job: all its fields as they are now, with the slices and the spend map cloned so that reading or encoding them while the job runs is safe.
func (l *live) snapshot() Job {
	l.mu.Lock()
	defer l.mu.Unlock()
	return cloned(l.job)
}

// set edits a live job under its lock, returning a safe copy of what it became.
func (l *live) set(edit func(*Job)) Job {
	l.mu.Lock()
	defer l.mu.Unlock()
	edit(&l.job)
	return cloned(l.job)
}

// elapsed is the wall time this job has spent working, over every run of it, with the stretches it spent waiting on the user taken out. Input: none. Output: the milliseconds, which is what the checkpoint carries as ElapsedMS.
func (l *live) elapsed() int64 {
	spent := time.Since(l.started) - time.Duration(l.waitedMS.Load())*time.Millisecond
	if since := l.waitingSince.Load(); since != 0 {
		spent -= time.Since(time.Unix(0, since))
	}
	return l.baseMS + spent.Milliseconds()
}

// beginWait marks the job as waiting on the user from now, and endWait folds the stretch it just waited into the time that does not count against the wall budget. endWait may be called twice; the second call does nothing.
func (l *live) beginWait() { l.waitingSince.Store(time.Now().UnixNano()) }

func (l *live) endWait() {
	if since := l.waitingSince.Swap(0); since != 0 {
		l.waitedMS.Add(time.Since(time.Unix(0, since)).Milliseconds())
	}
	select {
	case l.working <- struct{}{}:
	default:
	}
}

// wallGuard ends a job once the time it has spent actually working reaches its wall budget. It does that instead of a deadline on the job's own context because the time a job spends stuck on a question or held paused is the user's, not the job's, and a job must not be timed out for how long someone took to answer it. Input: the live job and the whole wall budget, what earlier runs of the job spent included. Output: none; it returns when the job's context is done.
func wallGuard(l *live, wall time.Duration) {
	t := time.NewTicker(wallTick)
	defer t.Stop()
	for {
		if l.waitingSince.Load() != 0 {
			// The job is waiting on the user, so elapsed() is frozen and no amount of ticking can bring it nearer the budget: sleep until the wait ends or the job does, rather than waking ten times a second for as long as the question goes unanswered.
			select {
			case <-l.ctx.Done():
				return
			case <-l.working:
			}
			continue
		}
		if time.Duration(l.elapsed())*time.Millisecond >= wall {
			l.cancel()
			return
		}
		select {
		case <-l.ctx.Done():
			return
		case <-t.C:
		}
	}
}

// waitWhilePaused holds the loop still while the job is paused, ending the wait and returning true when a resume arrives or false when the context is cancelled. Input: the job's context and the live job. Output: true to carry on, false to end.
func (l *live) waitWhilePaused(ctx context.Context) bool {
	for l.paused.Load() {
		l.beginWait()
		select {
		case <-l.resumes:
			l.endWait()
		case <-ctx.Done():
			l.endWait()
			return false
		}
	}
	return ctx.Err() == nil
}
