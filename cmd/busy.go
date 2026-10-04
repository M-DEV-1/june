package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"june/internal/actjob"
	"june/internal/agent"
	"june/internal/ipc"
	"june/internal/recorder"
)

// busyState is what the daemon is in the middle of for the user that a restart would cut short and that the June after it does not take up again: a voice conversation, a dictation, a computer task, and an answer being written. restartBlocker reads it, so POST /restart, setup's own restarts and the updater all wait for these as they wait for a recording. Each part is filled in where the thing it watches is made, and reads as idle until then.
type busyState struct {
	// asks counts the answers being written right now, through countedAsker.
	asks atomic.Int32
	// dictatingUntil is when an open dictation is taken to have ended by itself, in Unix nanoseconds, 0 when none is open (see trackDictation).
	dictatingUntil atomic.Int64
	// stopping counts POST /dictate/stop requests in flight, each of which is transcribing what was said.
	stopping atomic.Int32
	// voice reports whether a voice conversation is open; nil until the session is made.
	voice atomic.Pointer[func() bool]

	jobsMu sync.Mutex
	// jobs holds the computer tasks started and not yet ended, a paused one or one waiting on the user's answer included.
	jobs map[string]bool
}

// dictationHold is how long after a dictation starts it counts as open when no stop comes for it: the longest a dictation records (maxDictation in internal/ipc/dictate.go). One that ended by itself is then transcribed under recorder.GPUBusy, which restartBlocker reads as well.
const dictationHold = 2 * time.Minute

// busyWord names what busyState says a restart now would cut short, in restartBlocker's vocabulary: "voice", "dictating", "acting", "asking", or "" for none of them.
func (b *busyState) busyWord() string {
	if v := b.voice.Load(); v != nil && (*v)() {
		return "voice"
	}
	if b.stopping.Load() > 0 || time.Now().UnixNano() < b.dictatingUntil.Load() || recorder.GPUBusy() {
		return "dictating"
	}
	b.jobsMu.Lock()
	acting := len(b.jobs) > 0
	b.jobsMu.Unlock()
	if acting {
		return "acting"
	}
	if b.asks.Load() > 0 {
		return "asking"
	}
	return ""
}

// watchVoice has busyWord ask the voice session whether a conversation is open, through its own GET /voice/status handler, which is the one place that says so. Input: the session.
func (b *busyState) watchVoice(v *ipc.VoiceSession) {
	active := func() bool {
		rec := &capturedResponse{}
		req, err := http.NewRequest(http.MethodGet, "/voice/status", nil)
		if err != nil {
			return false
		}
		v.Status(rec, req)
		var status struct {
			Active bool `json:"active"`
		}
		return json.Unmarshal(rec.body.Bytes(), &status) == nil && status.Active
	}
	b.voice.Store(&active)
}

// trackDictation wraps the dictation routes so busyWord knows when one is open. Input: POST /dictate/start's and POST /dictate/stop's handlers. Output: the two wrapped.
// A dictation is open from a start that succeeded until a stop has transcribed it, or until dictationHold has passed for one that ended on its own and whose stop, if any, found nothing open.
func (b *busyState) trackDictation(start, stop http.HandlerFunc) (http.HandlerFunc, http.HandlerFunc) {
	wrappedStart := func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start(sw, r)
		if sw.status >= 200 && sw.status < 300 {
			b.dictatingUntil.Store(time.Now().Add(dictationHold).UnixNano())
		}
	}
	wrappedStop := func(w http.ResponseWriter, r *http.Request) {
		b.stopping.Add(1)
		defer func() {
			b.dictatingUntil.Store(0)
			b.stopping.Add(-1)
		}()
		stop(w, r)
	}
	return wrappedStart, wrappedStop
}

// trackJobs wraps the computer-task runner's event sink so busyWord knows which tasks are live: from "started" or "resumed" until "done", which the runner says on every way a task ends. Input: the sink the events go on to. Output: the wrapped sink.
func (b *busyState) trackJobs(emit func(actjob.Event)) func(actjob.Event) {
	return func(ev actjob.Event) {
		b.jobsMu.Lock()
		switch ev.Kind {
		case "started", "resumed":
			if b.jobs == nil {
				b.jobs = map[string]bool{}
			}
			b.jobs[ev.Job] = true
		case "done":
			delete(b.jobs, ev.Job)
		}
		b.jobsMu.Unlock()
		emit(ev)
	}
}

// countedAsker is an ipc.Asker that counts the answers it is writing in busyState.asks. It offers the two optional methods ipc looks for on an asker, the conversation so far and the end-of-ask lesson hook, and hands each on when the asker it wraps has it, doing what ipc itself does when it has not.
type countedAsker struct {
	inner ipc.Asker
	busy  *busyState
}

// AskText answers question through the wrapped asker, counted.
func (c countedAsker) AskText(ctx context.Context, question string) (agent.TurnTrace, error) {
	c.busy.asks.Add(1)
	defer c.busy.asks.Add(-1)
	return c.inner.AskText(ctx, question)
}

// AskTextWith answers question with the conversation so far, counted, or without it when the wrapped asker cannot read one.
func (c countedAsker) AskTextWith(ctx context.Context, history agent.History, question string) (agent.TurnTrace, error) {
	c.busy.asks.Add(1)
	defer c.busy.asks.Add(-1)
	if withHistory, ok := c.inner.(interface {
		AskTextWith(context.Context, agent.History, string) (agent.TurnTrace, error)
	}); ok {
		return withHistory.AskTextWith(ctx, history, question)
	}
	return c.inner.AskText(ctx, question)
}

// AfterScreenRun hands the finished trace to the wrapped asker's lesson hook, when it has one.
func (c countedAsker) AfterScreenRun(ctx context.Context, trace agent.TurnTrace, outcome string) {
	if hook, ok := c.inner.(interface {
		AfterScreenRun(context.Context, agent.TurnTrace, string)
	}); ok {
		hook.AfterScreenRun(ctx, trace, outcome)
	}
}

// capturedResponse is an http.ResponseWriter that keeps the body a handler wrote, for asking a route's own handler a question inside the daemon.
type capturedResponse struct {
	header http.Header
	body   bytes.Buffer
}

func (c *capturedResponse) Header() http.Header {
	if c.header == nil {
		c.header = http.Header{}
	}
	return c.header
}

func (c *capturedResponse) Write(p []byte) (int, error) { return c.body.Write(p) }

func (c *capturedResponse) WriteHeader(int) {}

// statusWriter passes a response through and keeps the status it was written with.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the writer underneath.
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }
