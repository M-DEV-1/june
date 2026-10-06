// agy_session.go keeps agy processes alive across asks instead of paying agy's own startup cost — login, model list, quota, experiments, a failing Playwright driver install — on every single question. Measured on 2026-09-10, that startup costs about 8 seconds before a 1-2 second model call; `agy --input-format stream-json --output-format stream-json` reads one line per turn from stdin and answers each in about 1-2 seconds once the process is already up.
// The processes are a small pool keyed by conversation rather than one Agent-wide slot. June does not run one ask at a time — the window, a routine and a job ask at once — and with one slot each ask queued behind the last with nothing on screen saying so (a two-word answer took 37 seconds on 2026-10-03), and every new conversation killed the process the previous one would have continued in. Each process remembers exactly one conversation; an ask whose conversation no live process holds gets a fresh one, idle processes past maxAgySessions are let go oldest first, and each one is killed after it sits unused past agyIdleTimeout.
package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"june/internal/util"
)

// agyIdleTimeout is how long a session's process may sit unused before it is killed. A package var so a test can shrink it to pay no real wall-clock cost.
var agyIdleTimeout = 10 * time.Minute

// maxAgySessions is how many idle agy processes are kept for conversations that may continue. More may run at once while asks overlap; the extra ones are let go as their turns end.
const maxAgySessions = 3

// agyExitWait bounds how long a killed process is waited for before its throwaway HOME is removed: the removal retries on its own, so this only spares it the first failed tries.
const agyExitWait = 10 * time.Second

// agySessionRunner is a live agy process: one line in over stdin is one turn, one stream of NDJSON lines out over stdout. agyProcess is the real implementation; a test replaces it with a stub so a whole conversation can be driven without the command line.
type agySessionRunner interface {
	// Start launches the process under env with args. Output: an error naming what could not be started.
	Start(ctx context.Context, env []string, args []string) error
	// Send writes one turn's line to the process's stdin. Output: an error when the write failed, which is this package's signal that the process has died.
	Send(line string) error
	// Events streams the raw NDJSON lines the process printed, closed when its stdout closes (which is also how a death mid-turn is noticed).
	Events() <-chan []byte
	// Kill stops the process. Safe to call more than once, on a process that never started, and while another goroutine is mid-turn on it.
	Kill()
}

// newAgyProcess builds the runner AskAgyWith actually uses.
func newAgyProcess() agySessionRunner { return &agyProcess{} }

// agyProcess is the real agySessionRunner, one long-lived `agy --input-format stream-json ...` subprocess.
type agyProcess struct {
	// dir is the working directory agy runs in, which it takes as its workspace; empty means the temp directory.
	dir string
	// stderr keeps the end of what the process printed on stderr, which is the only place agy says why it would not start; with stderr dropped, an agy that died on start reached the user as "write |1: The pipe has been ended." (2026-10-06).
	stderr tailBuffer
	events chan []byte
	// exited is closed once the process has been reaped and its process group let go, which is when the files it held in its throwaway HOME can be removed.
	exited chan struct{}
	// mu guards everything below, because a shutdown kills a process another goroutine may be sending a turn to.
	mu    sync.Mutex
	cmd   *exec.Cmd
	stdin io.WriteCloser
	// stop is closed by the first Kill, which tells the reader in Start that nobody may be reading events any more.
	stop   chan struct{}
	killed bool
	reaped bool
}

func (p *agyProcess) Start(ctx context.Context, env, args []string) error {
	cmd := exec.Command(agyBinary, args...)
	// Its own process group (a Job Object on Windows), so Kill reaches whatever agy has started as well, such as a command one of its tools ran.
	util.OwnProcessGroup(cmd)
	cmd.Env = env
	cmd.Dir = p.dir
	if cmd.Dir == "" {
		cmd.Dir = os.TempDir()
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("agy: opening the session's stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("agy: opening the session's stdout: %w", err)
	}
	cmd.Stderr = &p.stderr
	// A child agy started can hold stderr open after agy is gone; the reaper's Wait then stops waiting for it rather than keeping the HOME undeletable.
	cmd.WaitDelay = 2 * time.Second
	release, err := util.StartProcessGroup(cmd)
	if err != nil {
		return fmt.Errorf("agy: starting the session: %w", err)
	}
	stop := make(chan struct{})
	p.mu.Lock()
	p.cmd = cmd
	p.stdin = stdin
	p.stop = stop
	p.mu.Unlock()
	events := make(chan []byte, 16)
	p.events = events
	p.exited = make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			select {
			case events <- line:
			case <-stop:
				// Killed, and nobody may ever read events again: an ask that was stopped or timed out has already returned, and blocking on a full buffer here left the process unreaped, its Job Object open and its HOME undeletable for good. The rest of the output is read and dropped until the pipe closes.
			}
		}
		close(events)
		// A line longer than the scanner takes ends the loop while the process is still writing, so what is left is drained: the process can then reach its end once it is killed, and Wait below returns.
		io.Copy(io.Discard, stdout)
		// Reaped here, once stdout is drained, because Wait closes the pipe and must not race the reads; a process that dies on its own is reaped the same way as one that was killed.
		cmd.Wait()
		p.mu.Lock()
		p.reaped = true
		release()
		p.mu.Unlock()
		close(p.exited)
	}()
	return nil
}

func (p *agyProcess) Send(line string) error {
	p.mu.Lock()
	stdin := p.stdin
	p.mu.Unlock()
	if stdin == nil {
		return errors.New("agy: the session was never started")
	}
	_, err := stdin.Write([]byte(line + "\n"))
	return err
}

func (p *agyProcess) Events() <-chan []byte { return p.events }

// Exited is closed once the process has been reaped. Nil for a process that never started.
func (p *agyProcess) Exited() <-chan struct{} { return p.exited }

// stderrAtExit is the end of what a dead process printed on stderr, read once it has exited or bound has passed, so the lines it printed as it gave up are in it.
func (p *agyProcess) stderrAtExit(bound time.Duration) string {
	waitExited(p, bound)
	return p.stderr.String()
}

// agyStderrTail is how much of a session's stderr is kept: enough for the lines agy prints as it gives up, and a fixed cost however long the session runs.
const agyStderrTail = 16 << 10

// tailBuffer is an io.Writer that keeps only the last agyStderrTail bytes written to it. The zero value is ready to use.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuffer) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, b...)
	if over := len(t.buf) - agyStderrTail; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(b), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// Kill stops the process and everything it started without waiting for it; the reaper in Start notices the exit. Killing only agy itself left whatever it had spawned running, and on Windows nothing else would ever stop it.
func (p *agyProcess) Kill() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stdin != nil {
		p.stdin.Close()
	}
	// A process already reaped is never signalled: its group or pid may since belong to something else.
	if p.cmd == nil || p.killed || p.reaped {
		return
	}
	p.killed = true
	close(p.stop)
	util.KillProcessGroup(p.cmd)
}

// waitExited waits, at most bound, for proc to have exited when it can say so. Input: the killed process and the bound. Output: none.
func waitExited(proc agySessionRunner, bound time.Duration) {
	p, ok := proc.(interface{ Exited() <-chan struct{} })
	if !ok || p.Exited() == nil {
		return
	}
	select {
	case <-p.Exited():
	case <-time.After(bound):
	}
}

// agySessionArgs is the argument list for one long-lived `agy` session, run as AgyAskAgent, which buildAgyHome defines in the session's workspace. Input: the model to ask for ("" leaves --model off, keeping the CLI's own default). Output: the arguments.
func agySessionArgs(model string) []string {
	args := []string{"--input-format", "stream-json", "--output-format", "stream-json", "--disable-slash-commands", "--print-timeout", agyAskTimeout.String(), "--agent", AgyAskAgent}
	if model != "" {
		args = append(args, "--model", model)
	}
	return args
}

// agyUserLine is the one NDJSON line one turn's text becomes — the only shape agy's stream-json input reads.
func agyUserLine(text string) string {
	// A map of plain strings always marshals; the error is impossible to hit.
	b, _ := json.Marshal(map[string]any{"event": "user", "message": map[string]any{"role": "user", "content": text}})
	return string(b)
}

// agySessionEnvelope is one NDJSON line agy prints. Only the "result" event carries an answer; "init" and "step_update" are read and dropped.
type agySessionEnvelope struct {
	Event  string     `json:"event"`
	Result *agyResult `json:"result"`
}

// agySlot is one live agy process and what it remembers. busy, timer and doomed belong to the pool and are read and written under its lock. proc, server and tempHome are only ever written under that lock too, because CloseAgySession kills the process of a slot an ask is holding; the ask holding the slot reads them without it. The rest belongs to whichever ask has the slot busy, and to the pool only while it is not.
type agySlot struct {
	busy bool
	// doomed marks a slot CloseAgySession killed while an ask had it, so the ask lets it go rather than restarting it.
	doomed   bool
	lastUsed time.Time
	timer    *time.Timer

	proc     agySessionRunner
	model    string
	tempHome string
	server   *claudeToolServer
	// seen is how many entries of the conversation's history the live process already holds: the history it was seeded with plus two for every turn it has answered since. first is the text of the conversation's opening turn as the process was given it. Together they say whether a history handed in is the conversation this process remembers; a different one gets another process, since a live process that remembers another conversation would answer from it.
	seen  int
	first string
}

// agySessionState is the pool of agy processes an Agent keeps alive between asks.
type agySessionState struct {
	mu    sync.Mutex
	slots []*agySlot
}

// conversationKnown reports whether the slot's process already holds the history handed in. Input: the slot and the history of the ask about to run. Output: true when the history is exactly as long as what the process has seen and opens with the same turn.
func conversationKnown(s *agySlot, history History) bool {
	if len(history) != s.seen {
		return false
	}
	return s.seen == 0 || sameOpening(s.first, historyFirstText(history))
}

// sameOpening reports whether a conversation's stored opening turn is the one a process was given. The process may have been given more: the window sends the question with what was on screen ahead of it ("On screen: ...\n\n<question>") and stores only the question, so comparing the two exactly restarted the process on every follow-up of a question asked from the hover.
func sameOpening(given, stored string) bool {
	return given == stored || (stored != "" && strings.HasSuffix(given, "\n\n"+stored))
}

// historyFirstText is the text of a conversation's opening turn, "" for an empty history or one whose first entry carries no text.
func historyFirstText(history History) string {
	if len(history) == 0 || history[0] == nil {
		return ""
	}
	for _, part := range history[0].Parts {
		if part != nil && part.Text != "" {
			return part.Text
		}
	}
	return ""
}

// agyPromptLine builds the NDJSON line for one turn. Input: fresh (true for a session's first message, when the process remembers nothing yet), the persona instruction, the conversation so far, the turn's start time and recalled lines (sent only when fresh — a live process remembers an earlier turn's time and recall, and only the new question needs today's), the act-reference block, and the question. Output: one line for agy's stdin.
func agyPromptLine(fresh bool, instruction string, history History, start time.Time, injected []string, reference, question string) string {
	var prompt strings.Builder
	if fresh {
		prompt.WriteString(instruction + "\n\n" + agyToolNote + "\n\n")
		if thread := claudeThread(history); thread != "" {
			prompt.WriteString(thread + "\n")
		}
		prompt.WriteString(turnContext(start, injected) + "\n\n")
	}
	if reference != "" {
		prompt.WriteString(reference + "\n\n")
	}
	prompt.WriteString(question)
	return agyUserLine(prompt.String())
}

// resetToolServerForTurn points a session's tool server at the turn now starting and clears the step budget, annotation count and hop list the turn before left behind, so each turn gets its own fresh accounting instead of inheriting the whole session's. claudeToolServer's fields are unexported but this file is in the same package, so no change to claude.go is needed.
func resetToolServerForTurn(s *claudeToolServer, ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.askCtx = ctx
	s.annotations = 0
	s.steps = 0
	s.hops = nil
}

// claim hands the ask a slot: the idle one whose process already holds this conversation, or a new empty one the caller must start. Input: the model and the conversation so far. Output: the slot, now busy, and whether its process is reused.
func (pool *agySessionState) claim(model string, history History) (*agySlot, bool) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	for _, s := range pool.slots {
		if s.busy || s.proc == nil || s.model != model || s.seen == 0 || !conversationKnown(s, history) {
			continue
		}
		s.busy = true
		if s.timer != nil {
			s.timer.Stop()
			s.timer = nil
		}
		return s, true
	}
	s := &agySlot{busy: true}
	pool.slots = append(pool.slots, s)
	return s, false
}

// release ends an ask's hold on a slot. A slot whose turn failed is killed, so a turn it is still producing can never be read as the next ask's answer; a healthy one waits idle for its conversation to continue, and the oldest idle slots past maxAgySessions are let go. Input: the slot and whether its turn failed. Output: none.
func (pool *agySessionState) release(s *agySlot, broken bool) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	s.busy = false
	if broken || s.doomed || s.proc == nil {
		pool.dropLocked(s)
		return
	}
	s.lastUsed = time.Now()
	s.timer = time.AfterFunc(agyIdleTimeout, func() { pool.expire(s) })
	for {
		var oldest *agySlot
		idle := 0
		for _, o := range pool.slots {
			if o.busy {
				continue
			}
			idle++
			if oldest == nil || o.lastUsed.Before(oldest.lastUsed) {
				oldest = o
			}
		}
		if idle <= maxAgySessions {
			return
		}
		pool.dropLocked(oldest)
	}
}

// expire kills a slot that has sat idle past agyIdleTimeout, unless an ask claimed it in the meantime.
func (pool *agySessionState) expire(s *agySlot) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if s.busy {
		return
	}
	pool.dropLocked(s)
}

// doomed reports whether CloseAgySession has killed this slot while an ask held it.
func (pool *agySessionState) doomed(s *agySlot) bool {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return s.doomed
}

// dropLocked takes a slot out of the pool and stops it. Caller holds pool.mu. Output: closed once the slot's throwaway HOME is gone.
func (pool *agySessionState) dropLocked(s *agySlot) <-chan struct{} {
	for i, o := range pool.slots {
		if o == s {
			pool.slots = append(pool.slots[:i], pool.slots[i+1:]...)
			break
		}
	}
	return stopAgySlot(s)
}

// stopAgySlot kills a slot's process and tool server and removes its throwaway HOME once the process is gone, retrying while a file in it is still held. The caller holds the pool's lock. Output: closed once the HOME is removed or given up on.
func stopAgySlot(s *agySlot) <-chan struct{} {
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	proc, home := s.proc, s.tempHome
	if proc != nil {
		proc.Kill()
	}
	if s.server != nil {
		s.server.Close()
	}
	s.proc, s.server, s.tempHome, s.model, s.seen, s.first = nil, nil, "", "", 0, ""
	done := make(chan struct{})
	go func() {
		defer close(done)
		if home == "" {
			return
		}
		// Removed only after the process is gone: a file it still has mapped cannot be deleted on Windows, and removing straight after the kill left most homes half there.
		if proc != nil {
			waitExited(proc, agyExitWait)
		}
		removeAgyHome(home)
	}()
	return done
}

// startAgySlot gives a slot a freshly started process: a new tool server, a new throwaway HOME mirroring it, and a new `agy` subprocess under that HOME. Whatever process the slot had is stopped first. The caller has the slot busy.
func (a *Agent) startAgySlot(s *agySlot, newProc func() agySessionRunner, model string) error {
	pool := &a.agySess
	pool.mu.Lock()
	stopAgySlot(s)
	pool.mu.Unlock()
	server, err := a.startClaudeToolServer(context.Background())
	if err != nil {
		return err
	}
	realHome, err := os.UserHomeDir()
	if err != nil {
		server.Close()
		return fmt.Errorf("agy: finding the real home directory to mirror: %w", err)
	}
	tempHome, err := os.MkdirTemp(os.TempDir(), agySessionHomePrefix)
	if err != nil {
		server.Close()
		return fmt.Errorf("agy: making the session's own temp home: %w", err)
	}
	if err := buildAgyHome(realHome, tempHome, server.URL()); err != nil {
		server.Close()
		removeAgyHome(tempHome)
		return err
	}
	proc := newProc()
	if p, ok := proc.(*agyProcess); ok {
		p.dir = filepath.Join(tempHome, agyWorkspaceDir)
	}
	if err := proc.Start(context.Background(), agyEnv(tempHome), agySessionArgs(model)); err != nil {
		server.Close()
		removeAgyHome(tempHome)
		return fmt.Errorf("agy: %w", err)
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	s.proc = proc
	s.model = model
	s.seen = 0
	s.first = ""
	s.tempHome = tempHome
	s.server = server
	if s.doomed {
		// CloseAgySession ran while this process was starting, before the slot had anything on it to kill, so it is killed here rather than left to run its turn through the daemon's shutdown.
		stopAgySlot(s)
		return errors.New("agy: June is shutting down")
	}
	return nil
}

// CloseAgySession kills every agy process this agent keeps, including one an ask is using right now, and waits a moment for their throwaway HOMEs to go. Meant for the daemon's own shutdown path.
func (a *Agent) CloseAgySession() {
	pool := &a.agySess
	pool.mu.Lock()
	var removed []<-chan struct{}
	for _, s := range append([]*agySlot(nil), pool.slots...) {
		if s.busy {
			// The ask holding it sees its process end and lets the slot go; it is not restarted.
			s.doomed = true
			if s.proc != nil {
				s.proc.Kill()
			}
			continue
		}
		removed = append(removed, pool.dropLocked(s))
	}
	pool.mu.Unlock()
	deadline := time.After(2500 * time.Millisecond)
	for _, done := range removed {
		select {
		case <-done:
		case <-deadline:
			return
		}
	}
}

// errAgySessionGone marks a turn that failed because the session's process was gone: its stdin would not take the turn, or its stdout closed before the result. runAgyTurn reads it to say why (see agyGoneError).
var errAgySessionGone = errors.New("the agy session ended")

// sendAgyTurn sends one turn's line and reads the session's stream until its result event arrives. The caller has the slot busy.
func sendAgyTurn(ctx context.Context, s *agySlot, line string) (agyResult, error) {
	if err := s.proc.Send(line); err != nil {
		return agyResult{}, fmt.Errorf("agy: writing to the session: %w: %w", errAgySessionGone, err)
	}
	events := s.proc.Events()
	for {
		select {
		case <-ctx.Done():
			return agyResult{}, ctx.Err()
		case raw, ok := <-events:
			if !ok {
				return agyResult{}, fmt.Errorf("agy: %w before answering", errAgySessionGone)
			}
			var env agySessionEnvelope
			if err := json.Unmarshal(raw, &env); err != nil {
				continue // a line this parser does not recognise (agy has other event shapes) is not fatal — skip it.
			}
			if env.Event == "result" && env.Result != nil {
				return *env.Result, nil
			}
		}
	}
}

// agyGoneWait bounds how long a dead session's process is waited for before its stderr is read for the reason it gave.
const agyGoneWait = 3 * time.Second

// agyGoneError is the error a turn whose process was gone becomes, so the question is handed to the next brain with the reason rather than stopping on a broken pipe: what agy said on stderr as it stopped (see AgyStderrError), or ErrNoAnswer when it died without saying. Input: the dead process and the turn's error. Output: the error.
func agyGoneError(proc agySessionRunner, err error) error {
	if p, ok := proc.(interface{ stderrAtExit(time.Duration) string }); ok {
		if said := AgyStderrError(p.stderrAtExit(agyGoneWait)); said != nil {
			return said
		}
	}
	return fmt.Errorf("%w: %w", ErrNoAnswer, err)
}

// agyStartError is the error a session that could not be started becomes: ErrCouldNotRun, so the question goes on to the next brain, unless June is shutting down. The cause stays wrapped, so a missing command line still reads as exec.ErrNotFound. Input: the pool, the slot and startAgySlot's error. Output: the error.
func agyStartError(pool *agySessionState, s *agySlot, err error) error {
	if pool.doomed(s) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrCouldNotRun, err)
}

// addAgyTurn folds a follow-up turn's result into the first one's: the follow-up's status and answer, both turns' cost, and every tool either was refused.
func addAgyTurn(first, more agyResult) agyResult {
	out := more
	out.NumTurns = first.NumTurns + more.NumTurns
	out.Usage.InputTokens = first.Usage.InputTokens + more.Usage.InputTokens
	out.Usage.OutputTokens = first.Usage.OutputTokens + more.Usage.OutputTokens
	out.Usage.ThinkingTokens = first.Usage.ThinkingTokens + more.Usage.ThinkingTokens
	out.Usage.CacheReadTokens = first.Usage.CacheReadTokens + more.Usage.CacheReadTokens
	out.Usage.TotalTokens = first.Usage.TotalTokens + more.Usage.TotalTokens
	out.DeniedActions = append(append(out.DeniedActions[:0:0], first.DeniedActions...), more.DeniedActions...)
	return out
}

// runAgyTurn runs one turn of an ask on a process that holds its conversation, starting one when none does. A turn that ends SUCCESS with nothing said — agy's way of ending a turn whose model reached for a tool it may not use — is asked once more in the same process before it counts as no answer. Output: the run's result, the tool hops it made, whether it hit its step cap, or an error.
func (a *Agent) runAgyTurn(ctx context.Context, newProc func() agySessionRunner, model, instruction string, history History, start time.Time, injected []string, reference, question string) (agyResult, []ToolHop, bool, error) {
	pool := &a.agySess
	s, reused := pool.claim(model, history)
	broken := true
	defer func() { pool.release(s, broken) }()

	if !reused {
		if err := a.startAgySlot(s, newProc, model); err != nil {
			return agyResult{}, nil, false, agyStartError(pool, s, err)
		}
	}
	resetToolServerForTurn(s.server, ctx)
	// The sweep at daemon start takes a HOME nobody has touched for hours as dead, so a live one is touched on every turn.
	now := time.Now()
	os.Chtimes(s.tempHome, now, now)

	line := agyPromptLine(!reused, instruction, history, start, injected, reference, question)
	res, err := sendAgyTurn(ctx, s, line)
	if err != nil && reused && ctx.Err() == nil && !pool.doomed(s) {
		// The process this ask was reusing has died since the last turn (idle-killed by the CLI itself, crashed, or similar) — replace it transparently and give the new one the instruction and history again, exactly as a fresh process would get them.
		if serr := a.startAgySlot(s, newProc, model); serr != nil {
			return agyResult{}, nil, false, agyStartError(pool, s, serr)
		}
		reused = false
		resetToolServerForTurn(s.server, ctx)
		line = agyPromptLine(true, instruction, history, start, injected, reference, question)
		res, err = sendAgyTurn(ctx, s, line)
	}
	if err == nil && res.Status == "SUCCESS" && strings.TrimSpace(res.Response) == "" && !s.server.Capped() {
		slog.Info("agy: the turn ended without an answer, asking once more", "denied", res.deniedTools())
		var more agyResult
		if more, err = sendAgyTurn(ctx, s, agyUserLine(agyNudge(res.deniedTools()))); err == nil {
			res = addAgyTurn(res, more)
		}
	}
	RefreshAgyUsage()
	hops := s.server.Hops()
	capped := s.server.Capped()
	if errors.Is(err, errAgySessionGone) && ctx.Err() == nil && !pool.doomed(s) {
		err = agyGoneError(s.proc, err)
	}
	if err != nil {
		return agyResult{}, hops, capped, err
	}
	// A turn June will not file as this process's answer — a failed run, or one still empty after the nudge — ends the process too. The question goes on to another provider and that one's answer is what June stores, so the follow-up hands in a history whose answer this process never gave, and reusing it would answer from a conversation the user was not shown. A capped turn is kept: June files the cap message as its answer, and a "continue" wants the process that got that far.
	if !capped && (res.Status != "SUCCESS" || strings.TrimSpace(res.Response) == "") {
		return res, hops, capped, nil
	}
	broken = false
	// The process now holds this turn on top of whatever it was seeded with, and the seed is recorded on a fresh process so the next ask can tell this conversation from another. A follow-up nudge is not counted: June's own thread holds the question and the one answer, and that is what the next ask hands in.
	if !reused {
		s.seen = len(history)
		s.first = historyFirstText(history)
		if s.seen == 0 {
			s.first = question
		}
	}
	s.seen += 2
	return res, hops, capped, nil
}
