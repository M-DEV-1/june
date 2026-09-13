// agy_session.go keeps one agy process alive across asks instead of paying agy's own startup cost — login, model list, quota, experiments, a failing Playwright driver install — on every single question. Measured on 2026-09-10, that startup costs about 8 seconds before a 1-2 second model call; `agy --input-format stream-json --output-format stream-json` reads one line per turn from stdin and answers each in about 1-2 seconds once the process is already up.
// A session is one Agent-wide slot: started lazily on the first ask, reused for the next one, replaced when the model changes, restarted transparently when it has died or a write to it fails, and killed after it sits idle past agyIdleTimeout. Ora runs one ask at a time, so there is no conversation id to key a session by; the slot is simply whichever process last answered, which is what "the same conversation continues" means here.
package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// agyIdleTimeout is how long a session's process may sit unused before it is killed. A package var so a test can shrink it to pay no real wall-clock cost.
var agyIdleTimeout = 10 * time.Minute

// agySessionRunner is a live agy process: one line in over stdin is one turn, one stream of NDJSON lines out over stdout. agyProcess is the real implementation; a test replaces it with a stub so a whole conversation can be driven without the command line.
type agySessionRunner interface {
	// Start launches the process under env with args. Output: an error naming what could not be started.
	Start(ctx context.Context, env []string, args []string) error
	// Send writes one turn's line to the process's stdin. Output: an error when the write failed, which is this package's signal that the process has died.
	Send(line string) error
	// Events streams the raw NDJSON lines the process printed, closed when its stdout closes (which is also how a death mid-turn is noticed).
	Events() <-chan []byte
	// Kill stops the process. Safe to call more than once and on a process that never started.
	Kill()
}

// newAgyProcess builds the runner AskAgyWith actually uses.
func newAgyProcess() agySessionRunner { return &agyProcess{} }

// agyProcess is the real agySessionRunner, one long-lived `agy --input-format stream-json ...` subprocess.
type agyProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	events chan []byte
}

func (p *agyProcess) Start(ctx context.Context, env, args []string) error {
	cmd := exec.Command(agyBinary, args...)
	cmd.Env = env
	cmd.Dir = os.TempDir()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("agy: opening the session's stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("agy: opening the session's stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("agy: starting the session: %w", err)
	}
	p.cmd = cmd
	p.stdin = stdin
	events := make(chan []byte, 16)
	p.events = events
	go func() {
		defer close(events)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			events <- line
		}
	}()
	return nil
}

func (p *agyProcess) Send(line string) error {
	if p.stdin == nil {
		return errors.New("agy: the session was never started")
	}
	_, err := p.stdin.Write([]byte(line + "\n"))
	return err
}

func (p *agyProcess) Events() <-chan []byte { return p.events }

// Kill stops the process without waiting for its grandchildren to let go of its pipes — the same concern runAgyCLI's WaitDelay guards against on the one-shot path, handled here by reaping in the background instead.
func (p *agyProcess) Kill() {
	if p.stdin != nil {
		p.stdin.Close()
	}
	if p.cmd == nil {
		return
	}
	if p.cmd.Process != nil {
		p.cmd.Process.Kill()
	}
	go p.cmd.Wait()
}

// agySessionArgs is the argument list for one long-lived `agy` session. Input: the model to ask for ("" leaves --model off, keeping the CLI's own default). Output: the arguments.
func agySessionArgs(model string) []string {
	args := []string{"--input-format", "stream-json", "--output-format", "stream-json", "--disable-slash-commands", "--print-timeout", agyAskTimeout.String()}
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

// agySessionState is the one agy process an Agent keeps alive between asks.
type agySessionState struct {
	mu       sync.Mutex
	proc     agySessionRunner
	model    string
	tempHome string
	server   *claudeToolServer
	timer    *time.Timer
	// seen is how many entries of the conversation's history the live process already holds: the history it was seeded with plus two for every turn it has answered since. first is the text of the conversation's opening turn. Together they say whether a history handed in is the conversation this process remembers; a different one gets a fresh process, since a live process that remembers another conversation would answer from it.
	seen  int
	first string
}

// conversationKnown reports whether the live process already holds the history handed in. Input: the session and the history of the ask about to run. Output: true when the history is exactly as long as what the process has seen and opens with the same turn.
func conversationKnown(sess *agySessionState, history History) bool {
	if len(history) != sess.seen {
		return false
	}
	return sess.seen == 0 || historyFirstText(history) == sess.first
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
		prompt.WriteString(instruction + "\n\n")
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

// startAgySessionLocked replaces sess's process with a freshly started one: a new tool server, a new throwaway HOME mirroring it, and a new `agy` subprocess under that HOME. Caller holds sess.mu.
func (a *Agent) startAgySessionLocked(sess *agySessionState, newProc func() agySessionRunner, model string) error {
	killAgySessionLocked(sess)
	server, err := a.startClaudeToolServer(context.Background())
	if err != nil {
		return err
	}
	realHome, err := os.UserHomeDir()
	if err != nil {
		server.Close()
		return fmt.Errorf("agy: finding the real home directory to mirror: %w", err)
	}
	tempHome, err := os.MkdirTemp(os.TempDir(), "ora-agy-session-")
	if err != nil {
		server.Close()
		return fmt.Errorf("agy: making the session's own temp home: %w", err)
	}
	if err := buildAgyHome(realHome, tempHome, server.URL()); err != nil {
		server.Close()
		os.RemoveAll(tempHome)
		return err
	}
	proc := newProc()
	if err := proc.Start(context.Background(), agyEnv(tempHome), agySessionArgs(model)); err != nil {
		server.Close()
		os.RemoveAll(tempHome)
		return fmt.Errorf("agy: %w", err)
	}
	sess.proc = proc
	sess.model = model
	sess.seen = 0
	sess.first = ""
	sess.tempHome = tempHome
	sess.server = server
	return nil
}

// killAgySessionLocked stops sess's process and everything built for it. Caller holds sess.mu. Safe on an empty sess.
func killAgySessionLocked(sess *agySessionState) {
	if sess.timer != nil {
		sess.timer.Stop()
		sess.timer = nil
	}
	if sess.proc != nil {
		sess.proc.Kill()
		sess.proc = nil
	}
	if sess.server != nil {
		sess.server.Close()
		sess.server = nil
	}
	if sess.tempHome != "" {
		os.RemoveAll(sess.tempHome)
		sess.tempHome = ""
	}
	sess.model = ""
}

// armAgySessionIdleTimerLocked starts the clock on sess sitting unused. Caller holds sess.mu; the callback takes the lock itself.
func (a *Agent) armAgySessionIdleTimerLocked(sess *agySessionState) {
	sess.timer = time.AfterFunc(agyIdleTimeout, func() {
		sess.mu.Lock()
		defer sess.mu.Unlock()
		killAgySessionLocked(sess)
	})
}

// CloseAgySession kills this agent's live agy process, if any. Meant for the daemon's own shutdown path.
func (a *Agent) CloseAgySession() {
	a.agySess.mu.Lock()
	defer a.agySess.mu.Unlock()
	killAgySessionLocked(&a.agySess)
}

// sendAgyTurnLocked sends one turn's line and reads the session's stream until its result event arrives. Caller holds sess.mu.
func sendAgyTurnLocked(ctx context.Context, sess *agySessionState, line string) (agyResult, error) {
	if err := sess.proc.Send(line); err != nil {
		return agyResult{}, fmt.Errorf("agy: writing to the session: %w", err)
	}
	events := sess.proc.Events()
	for {
		select {
		case <-ctx.Done():
			return agyResult{}, ctx.Err()
		case raw, ok := <-events:
			if !ok {
				return agyResult{}, errors.New("agy: the session ended before answering")
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

// runAgyTurn runs one turn of an ask against this agent's live session, starting or restarting it as needed. Output: the run's result, the tool hops it made, whether it hit its step cap, or an error.
func (a *Agent) runAgyTurn(ctx context.Context, newProc func() agySessionRunner, model, instruction string, history History, start time.Time, injected []string, reference, question string) (agyResult, []ToolHop, bool, error) {
	sess := &a.agySess
	sess.mu.Lock()
	defer sess.mu.Unlock()

	reused := sess.proc != nil && sess.model == model && conversationKnown(sess, history)
	if reused {
		if sess.timer != nil {
			sess.timer.Stop()
			sess.timer = nil
		}
	} else if err := a.startAgySessionLocked(sess, newProc, model); err != nil {
		return agyResult{}, nil, false, err
	}
	resetToolServerForTurn(sess.server, ctx)

	line := agyPromptLine(!reused, instruction, history, start, injected, reference, question)
	res, err := sendAgyTurnLocked(ctx, sess, line)
	if err != nil && reused {
		// The process this ask was reusing has died since the last turn (idle-killed by the CLI itself, crashed, or similar) — replace it transparently and give the new one the instruction and history again, exactly as a fresh process would get them.
		if serr := a.startAgySessionLocked(sess, newProc, model); serr != nil {
			return agyResult{}, nil, false, serr
		}
		resetToolServerForTurn(sess.server, ctx)
		line = agyPromptLine(true, instruction, history, start, injected, reference, question)
		res, err = sendAgyTurnLocked(ctx, sess, line)
	}
	hops := sess.server.Hops()
	capped := sess.server.Capped()
	if err != nil {
		return agyResult{}, hops, capped, err
	}
	a.armAgySessionIdleTimerLocked(sess)
	// The process now holds this turn on top of whatever it was seeded with, and the seed is recorded on a fresh process so the next ask can tell this conversation from another.
	if !reused {
		sess.seen = len(history)
		sess.first = historyFirstText(history)
		if sess.seen == 0 {
			sess.first = question
		}
	}
	sess.seen += 2
	return res, hops, capped, nil
}
