package brain

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"june/internal/agent"
	"june/internal/util"
)

// ClaudeCLI answers by running `claude -p`, which uses whatever Claude Code login the machine already has — a subscription, billed as a subscription, not per-token API calls.
// --bare is deliberately never passed: it makes the CLI read ANTHROPIC_API_KEY instead of the login, which is the billing this whole path exists to avoid.
// --restricted, --strict-mcp-config, and an empty --tools list are always passed, because the prompt carries text nobody vetted (a meeting transcript, whatever was on the user's screens) and this duty needs no tools at all — --restricted alone still leaves file tools available, so the empty tool list is what actually closes the door.
// The prompt goes in on stdin: a single argv entry is capped at 128 KB on Linux and a long meeting is bigger than that.
// Input: the path to the binary, the model to ask for ("" = whatever the login defaults to), and a hard timeout in seconds. Output: the "result" field of the CLI's JSON.
func ClaudeCLI(binary, model string, timeoutSeconds int) Brain {
	return func(ctx context.Context, prompt string) (string, error) {
		args := []string{"-p", "--output-format", "json", "--restricted", "--strict-mcp-config", "--tools", ""}
		if model != "" {
			args = append(args, "--model", model)
		}
		out, err := runCLI(ctx, binary, timeoutSeconds, args, prompt, wholeJSON, cliPlace{})
		if err != nil {
			return "", err
		}
		var res struct {
			Result  string `json:"result"`
			IsError bool   `json:"is_error"`
			Subtype string `json:"subtype"`
		}
		if err := json.Unmarshal(out, &res); err != nil {
			slog.Debug("claude -p printed something that is not JSON", "output", util.LogHead(string(out)))
			return "", fmt.Errorf("could not parse the output of claude -p: %w", err)
		}
		if res.IsError {
			slog.Debug("claude -p reported a failure", "subtype", res.Subtype, "result", util.LogHead(res.Result))
			// An expired login is told apart from every other failure, without the CLI's own words: it is the one another provider can fix, and the router hands a duty on only from a failure that says so.
			if err := agent.ClaudeLoginFailed(res.Result); err != nil {
				return "", err
			}
			// The CLI sets subtype "success" on a failed run too, and "claude -p failed (success)" was all a failed job ever said (2026-10-03).
			if res.Subtype == "" || res.Subtype == "success" {
				return "", errors.New("claude -p reported a failure; its own message is in the debug log")
			}
			return "", fmt.Errorf("claude -p failed (%s)", res.Subtype)
		}
		agent.ClaudeSignedInAgain()
		text := strings.TrimSpace(res.Result)
		if text == "" {
			return "", fmt.Errorf("claude -p returned no text")
		}
		return text, nil
	}
}

// agyTextOnly opens every prompt an agy duty sends. A duty runs as agent.AgyDutyAgent, which is offered no tool, and the grant file in its throwaway HOME refuses whatever it reaches for should agy ever run it as the default agent instead (see agent.AgyDutyHome); this says the same in words. It speaks of the model's own tools only, because a job round is an agy duty too and must answer with JSON naming one of June's tools: "use no tools" read literally could turn that round into a refusal. agyTextOnlyAgain opens the one retry a duty gets when the first run ended with nothing said.
const (
	agyTextOnly      = "Answer the request below with text alone. Do not call any tool of your own: none is available to this request, and any you call is refused. Where the request asks you to name a tool or an action, or to reply in JSON, write that out as your text.\n\n"
	agyTextOnlyAgain = "An earlier attempt at the request below ended without an answer because it called a tool of its own. Answer it now with text alone, calling no tool; where it asks you to name a tool or an action, or to reply in JSON, write that out as your text.\n\n"
)

// AgyCLI answers by running Antigravity's `agy` in print mode, under the Antigravity login the machine already has — a paid plan whose default model is a pro tier, which is why it earns a place beside claude while the local grinder is still being perfected.
// The prompt goes in on stdin as one stream-json turn. --print takes the prompt only as its own value (agy 1.2.15 refuses an empty one and does not read stdin for it, checked 2026-10-03), and as an argument a prompt is capped at 128 KB on Linux and the whole command line at 32,767 characters on Windows, which a day summary or a meeting's minutes pass. stream-json input requires stream-json output, so the answer is the one "result" event among the lines agy prints, and that event carries the same object --print --output-format json prints.
// The timeout matters more here than for claude: agy has open bugs where a print run with no terminal attached never returns.
// Each run gets a throwaway HOME of its own with no MCP server and every kind of tool refused, and runs as a custom agent offered no tool at all in a directory of its own: under the user's real HOME and the temp directory, a duty was offered about sixty tools, its workspace was the whole of %TEMP%, and a model reaching for run_command ended the round with an empty answer that failed the job (2026-10-03).
// Input: the path to the binary and a hard timeout in seconds. Output: the "response" field of the CLI's result.
func AgyCLI(binary string, timeoutSeconds int, model ...string) Brain {
	return func(ctx context.Context, prompt string) (string, error) {
		text, denied, err := agyDuty(ctx, binary, timeoutSeconds, first(model), agyTextOnly+prompt)
		if err == nil && text == "" && ctx.Err() == nil {
			slog.Info("agy: a duty ended without an answer, asking once more", "denied", denied)
			text, denied, err = agyDuty(ctx, binary, timeoutSeconds, first(model), agyTextOnlyAgain+prompt)
		}
		if err != nil {
			return "", err
		}
		if text == "" {
			note := ""
			if denied != "" {
				note = " (it reached for tools it may not use here: " + denied + ")"
			}
			return "", fmt.Errorf("%w: agy returned no text%s", agent.ErrNoAnswer, note)
		}
		agent.RefreshAgyUsage()
		return text, nil
	}
}

// agyDuty is one agy duty run in a throwaway HOME of its own. Input: the caller's context, the binary, the timeout in seconds, the model ("" for the CLI's own) and the whole prompt. Output: the answer (empty when the run ended with nothing said), the tools agy refused on the way, or an error — wrapping agent.ErrLoggedOut when the reason agy gave is its own login having expired, so the router hands the duty on.
func agyDuty(ctx context.Context, binary string, timeoutSeconds int, model, prompt string) (text, denied string, err error) {
	env, dir, remove, err := agent.AgyDutyHome()
	if err != nil {
		return "", "", fmt.Errorf("%w: %w", agent.ErrCouldNotRun, err)
	}
	defer remove()
	out, err := runCLI(ctx, binary, timeoutSeconds, agyArgs(model), agyTurn(prompt), agyResultEvent, cliPlace{env: env, dir: dir})
	if err != nil {
		return "", "", agyRunError(err)
	}
	var res struct {
		Status        string `json:"status"`
		Response      string `json:"response"`
		Error         string `json:"error"`
		DeniedActions []struct {
			Action      string `json:"action"`
			DisplayName string `json:"display_name"`
		} `json:"denied_actions"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		slog.Debug("agy printed something that is not JSON", "output", util.LogHead(string(out)))
		return "", "", fmt.Errorf("could not parse the output of agy: %w", err)
	}
	if res.Status != "SUCCESS" {
		slog.Debug("agy reported a failure", "status", res.Status, "error", util.LogHead(res.Error), "response", util.LogHead(res.Response))
		reason := res.Error
		if reason == "" {
			reason = res.Response
		}
		if agent.AgyLoginExpired(reason) {
			return "", "", fmt.Errorf("%w: agy failed with status %s: the Antigravity login has expired", agent.ErrLoggedOut, res.Status)
		}
		// agy's own error field is its message about the run, not the model's text, so its head goes in the error: "agy failed with status ERROR" was all a failed job said (2026-10-03). The response is left out, because on a failed run it can be a half-written answer quoting the prompt.
		if res.Error != "" {
			return "", "", fmt.Errorf("agy failed with status %s: %s", res.Status, util.RunesEllipsis(util.OneLine(res.Error), 160))
		}
		return "", "", fmt.Errorf("agy failed with status %s and gave no reason", res.Status)
	}
	var names []string
	for _, d := range res.DeniedActions {
		name := cmp.Or(d.DisplayName, d.Action)
		if name != "" && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return strings.TrimSpace(res.Response), strings.Join(names, ", "), nil
}

// agyRunError is the error an agy duty run that exited before answering becomes, so the duty is handed to the next brain with a reason rather than stopping on "agy: exit status 1", which is all four failed duties of 2026-10-06 said: what agy said on stderr as it stopped (see agent.AgyStderrError), or agent.ErrNoAnswer when it died without saying. Input: runCLI's error. Output: the error; a timeout or a stop is returned as it came.
func agyRunError(err error) error {
	var exit *cliExitError
	if !errors.As(err, &exit) {
		return err
	}
	if said := agent.AgyStderrError(exit.stderr); said != nil {
		return said
	}
	return fmt.Errorf("%w: agy stopped before it answered: %w", agent.ErrNoAnswer, err)
}

// GrokCLI answers by running `grok -p`, under the Grok login the machine already has — also a paid plan with a pro default model. Every tool is denied ('--deny *'), because the prompt carries text nobody vetted and these duties need none.
// The prompt is an argv entry: grok -p reads no prompt from stdin (its documented headless flags at docs.x.ai, checked 2026-10-03, take it only as -p's value; the one stdin route is `grok agent stdio`, a JSON-RPC agent protocol). That caps a prompt at 128 KB on Linux and the whole command line at 32,767 characters on Windows, and the npm package installs grok as a grok.cmd shim that cmd.exe re-reads, where a quote in the prompt ends the argument and the rest runs as a command. runCLI refuses either case before starting anything, rather than run a prompt that would arrive cut short or as something else.
// Input: the path to the binary and a hard timeout in seconds. Output: the "text" field of the CLI's JSON.
func GrokCLI(binary string, timeoutSeconds int, model ...string) Brain {
	return func(ctx context.Context, prompt string) (string, error) {
		out, err := runCLI(ctx, binary, timeoutSeconds, grokArgs(prompt, first(model)), "", wholeJSON, cliPlace{})
		if errors.Is(err, util.ErrBatchArgument) {
			return "", fmt.Errorf("%w; the native grok build (irm https://x.ai/cli/install.ps1 | iex) takes a prompt like this one, the npm package cannot", err)
		}
		if err != nil {
			return "", err
		}
		var res struct {
			Text       string `json:"text"`
			StopReason string `json:"stopReason"`
		}
		if err := json.Unmarshal(out, &res); err != nil {
			slog.Debug("grok -p printed something that is not JSON", "output", util.LogHead(string(out)))
			return "", fmt.Errorf("could not parse the output of grok -p: %w", err)
		}
		text := strings.TrimSpace(res.Text)
		if text == "" {
			return "", fmt.Errorf("grok -p returned no text (stop reason %s)", res.StopReason)
		}
		return text, nil
	}
}

// agyArgs is the argument list for one agy print-mode run that reads its prompt from stdin, as the tool-less agent agent.AgyDutyHome defines in the run's workspace. An empty model leaves --model off, which is what keeps the CLI's own default.
func agyArgs(model string) []string {
	args := []string{"--input-format", "stream-json", "--output-format", "stream-json", "--disable-slash-commands", "--agent", agent.AgyDutyAgent}
	if model != "" {
		args = append(args, "--model", model)
	}
	return args
}

// agyTurn is the stdin of one agy run: the prompt as the single NDJSON user turn stream-json input reads. agy answers it, sees stdin end and exits.
func agyTurn(prompt string) string {
	// A map of plain strings always marshals; the error is impossible to hit.
	b, _ := json.Marshal(map[string]any{"event": "user", "message": map[string]any{"role": "user", "content": prompt}})
	return string(b) + "\n"
}

// agyResultEvent finds the answer in what agy has printed so far: the object its "result" event carries, which arrives after the "init" and "step_update" lines. Output: that object and true, or false while no whole result line has arrived.
func agyResultEvent(b []byte) ([]byte, bool) {
	for line := range bytes.Lines(b) {
		// This runs on every read of a stream that can be thousands of step_update lines long, so only a line that can be the result event is decoded.
		if !bytes.Contains(line, []byte(`"result"`)) {
			continue
		}
		var event struct {
			Event  string          `json:"event"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(line, &event) == nil && event.Event == "result" && len(event.Result) > 0 {
			return event.Result, true
		}
	}
	return nil, false
}

// wholeJSON is the answer of a CLI that prints one JSON value: all of stdout, once it reads as one.
func wholeJSON(b []byte) ([]byte, bool) {
	return b, completeJSON(b)
}

// grokArgs is the argument list for one grok -p run. An empty model leaves -m off.
func grokArgs(prompt, model string) []string {
	args := []string{"-p", prompt, "--output-format", "json", "--deny", "*"}
	if model != "" {
		args = append(args, "-m", model)
	}
	return args
}

// first is the caller's optional model name, or empty when they passed none.
func first(ss []string) string {
	if len(ss) > 0 {
		return ss[0]
	}
	return ""
}

// runCLI runs one child process under a hard timeout and returns its answer. It returns as soon as stdout holds the whole answer — one complete JSON value for claude and grok, the result event for agy — and does not wait for the child to exit: on 2026-09-08 a meeting's minutes were lost to "claude timed out after 5m0s" when the CLI had printed its result and then sat there. The child and everything it spawned are killed once the answer is in hand or the timeout passes.
// Input: the binary, the timeout in seconds, the arguments, what to feed the child on stdin, and answer, which picks the answer out of the stdout read so far and reports whether it is all there. Output: the answer (all of stdout when the child exits cleanly without one), or an error naming a command line Windows would not deliver intact, the timeout (with the head of the child's stderr, since a run that says nothing else is otherwise untraceable), the exit status, or the stderr the child died with.
// The CLIs' own output is logged at debug rather than returned in the error, because these errors are logged at warn by callers such as the evening close (slog.Warn("evening close failed", "error", err)) and the output can carry part of the prompt — a day of the user's screen text — or a login error naming their account.
// A caller that is stopped (its context cancelled, as a job's Stop does) gets the context's own error back, wrapped, and a caller already stopped starts nothing: the Stop of a job 2.7 seconds in was logged as "agy timed out after 5m0s" and its fallback brain was started and killed on the spot (2026-10-03).
func runCLI(ctx context.Context, binary string, timeoutSeconds int, args []string, stdin string, answer func([]byte) ([]byte, bool), place cliPlace) ([]byte, error) {
	name := filepath.Base(binary)
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: not started, the caller had already stopped: %w", name, err)
	}
	parent := ctx
	timeout := time.Duration(timeoutSeconds) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.Command(binary, args...)
	if err := util.CheckCommandLine(cmd); err != nil {
		return nil, err
	}
	cmd.Stdin = strings.NewReader(stdin)
	// Both CLIs read project files, per-directory settings and instruction files out of their working directory, and the daemon's working directory is wherever the user happened to launch it from. An empty temporary directory makes one run look like every other.
	cmd.Dir = os.TempDir()
	if place.dir != "" {
		cmd.Dir = place.dir
	}
	cmd.Env = place.env
	// Its own process group (a Job Object on Windows), so the kill below reaches the children a CLI spawns as well as the CLI, and none of them can hold stdout open after the CLI itself is gone.
	util.OwnProcessGroup(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	release, err := util.StartProcessGroup(cmd)
	if err != nil {
		return nil, err
	}
	defer release()
	kill := func() { util.KillProcessGroup(cmd) }

	// stdout is read on its own goroutine, which hands back the answer as soon as it is all there, or what was read once the pipe closes, so the wait below can be cut short by either the answer or the clock.
	type read struct {
		out      []byte
		complete bool
	}
	done := make(chan read, 1)
	go func() {
		var out bytes.Buffer
		buf := make([]byte, 32<<10)
		for {
			n, err := stdout.Read(buf)
			out.Write(buf[:n])
			if ans, ok := answer(out.Bytes()); ok {
				done <- read{out: ans, complete: true}
				return
			}
			if err != nil {
				done <- read{out: out.Bytes()}
				return
			}
		}
	}()

	select {
	case r := <-done:
		if r.complete {
			kill()
			cmd.Wait()
			return r.out, nil
		}
		// The pipe closed without a whole answer: the child is done (or dying), and its exit status says how.
		kill()
		if err := cmd.Wait(); err != nil {
			slog.Debug("a brain CLI exited with an error", "binary", name, "stderr", util.LogHead(stderr.String()))
			return nil, &cliExitError{name: name, err: err, stderr: stderr.String()}
		}
		return r.out, nil
	case <-ctx.Done():
		kill()
		cmd.Wait()
		if err := parent.Err(); err != nil {
			return nil, fmt.Errorf("%s: stopped: %w", name, err)
		}
		// The reader hands back what it read once the kill closes the pipe; the bound is for a pipe something outside the process group still holds.
		var printed []byte
		select {
		case r := <-done:
			printed = r.out
		case <-time.After(2 * time.Second):
		}
		return nil, fmt.Errorf("%s timed out after %s (stderr: %s; stdout: %s)", name, timeout, util.LogHead(stderr.String()), printedShape(printed))
	}
}

// printedShape describes what a child had printed on stdout when it was killed, with none of the text in it: how much, and the event its last whole line was with the names of the fields that event carried. A timed-out agy run with nothing on stderr said nothing else, and one that had printed nothing is a different fault from one still streaming its answer: on 2026-10-06 agy's own log went quiet 17 seconds into a dreaming duty, yet its conversation store was written to until 17 seconds before June's five-minute limit, and June's log said only "agy timed out after 5m0s (stderr: )". Input: stdout so far. Output: a short description for a log line.
func printedShape(out []byte) string {
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		return "nothing"
	}
	lines := bytes.Split(out, []byte("\n"))
	shape := fmt.Sprintf("%d lines, %d bytes", len(lines), len(out))
	// The last line can be cut off mid-write by the kill, so the last one that reads as an event is the one described.
	for i := len(lines) - 1; i >= 0; i-- {
		var line map[string]json.RawMessage
		var event string
		if json.Unmarshal(lines[i], &line) != nil || json.Unmarshal(line["event"], &event) != nil || event == "" {
			continue
		}
		var fields map[string]json.RawMessage
		json.Unmarshal(line[event], &fields)
		names := make([]string, 0, len(fields))
		for k := range fields {
			names = append(names, k)
		}
		slices.Sort(names)
		return fmt.Sprintf("%s, the last event %q, line %d [%s]", shape, util.RunesEllipsis(event, 40), i+1, util.RunesEllipsis(strings.Join(names, " "), 120))
	}
	return shape + ", no event line"
}

// cliExitError is a child that exited with an error before giving a whole answer. Its text is the exit status alone, for the reason runCLI gives; the stderr is kept beside it for a caller that reads its own CLI's words out of it (see agyRunError), and Error never prints it.
type cliExitError struct {
	name   string
	err    error
	stderr string
}

func (e *cliExitError) Error() string { return e.name + ": " + e.err.Error() }

func (e *cliExitError) Unwrap() error { return e.err }

// cliPlace is where and under what environment runCLI starts a child. The zero value is the daemon's own environment in the temp directory, which is what claude and grok get; an agy duty gets a throwaway HOME and an empty workspace of its own.
type cliPlace struct {
	// env is the child's whole environment, nil for the daemon's own.
	env []string
	// dir is the child's working directory, "" for the temp directory.
	dir string
}

// completeJSON reports whether b, ignoring surrounding whitespace, is one whole JSON value: the moment a CLI's stdout reads this way its answer is in hand. Output: false for empty or partial output.
func completeJSON(b []byte) bool {
	b = bytes.TrimSpace(b)
	return len(b) > 0 && json.Valid(b)
}
