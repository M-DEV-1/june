package brain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"ora/internal/util"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
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
		out, err := runCLI(ctx, binary, timeoutSeconds, args, prompt)
		if err != nil {
			return "", err
		}
		var res struct {
			Result  string `json:"result"`
			IsError bool   `json:"is_error"`
			Subtype string `json:"subtype"`
		}
		if err := json.Unmarshal(out, &res); err != nil {
			slog.Debug("claude -p printed something that is not JSON", "output", head(string(out)))
			return "", fmt.Errorf("could not parse the output of claude -p: %w", err)
		}
		if res.IsError {
			slog.Debug("claude -p reported a failure", "subtype", res.Subtype, "result", head(res.Result))
			return "", fmt.Errorf("claude -p failed (%s)", res.Subtype)
		}
		text := strings.TrimSpace(res.Result)
		if text == "" {
			return "", fmt.Errorf("claude -p returned no text")
		}
		return text, nil
	}
}

// AgyCLI answers by running Antigravity's `agy --print`, under the Antigravity login the machine already has — a paid plan whose default model is a pro tier, which is why it earns a place beside claude while the local grinder is still being perfected.
// The prompt is the value of --print rather than stdin, which is the only text input that CLI takes; that caps a prompt at one argv entry, 128 KB on Linux.
// The timeout matters more here than for claude: agy has open bugs where a print run with no terminal attached never returns.
// Input: the path to the binary and a hard timeout in seconds. Output: the "response" field of the CLI's JSON.
func AgyCLI(binary string, timeoutSeconds int, model ...string) Brain {
	return func(ctx context.Context, prompt string) (string, error) {
		out, err := runCLI(ctx, binary, timeoutSeconds, agyArgs(prompt, first(model)), "")
		if err != nil {
			return "", err
		}
		var res struct {
			Status   string `json:"status"`
			Response string `json:"response"`
		}
		if err := json.Unmarshal(out, &res); err != nil {
			slog.Debug("agy --print printed something that is not JSON", "output", head(string(out)))
			return "", fmt.Errorf("could not parse the output of agy --print: %w", err)
		}
		if res.Status != "SUCCESS" {
			slog.Debug("agy --print reported a failure", "status", res.Status, "response", head(res.Response))
			return "", fmt.Errorf("agy --print failed with status %s", res.Status)
		}
		text := strings.TrimSpace(res.Response)
		if text == "" {
			return "", fmt.Errorf("agy --print returned no text")
		}
		return text, nil
	}
}

// GrokCLI answers by running `grok -p`, under the Grok login the machine already has — also a paid plan with a pro default model. Every tool is denied ('--deny *'), because the prompt carries text nobody vetted and these duties need none.
// The prompt is an argv entry (grok takes no stdin prompt), capping it at 128 KB on Linux.
// Input: the path to the binary and a hard timeout in seconds. Output: the "text" field of the CLI's JSON.
func GrokCLI(binary string, timeoutSeconds int, model ...string) Brain {
	return func(ctx context.Context, prompt string) (string, error) {
		out, err := runCLI(ctx, binary, timeoutSeconds, grokArgs(prompt, first(model)), "")
		if err != nil {
			return "", err
		}
		var res struct {
			Text       string `json:"text"`
			StopReason string `json:"stopReason"`
		}
		if err := json.Unmarshal(out, &res); err != nil {
			slog.Debug("grok -p printed something that is not JSON", "output", head(string(out)))
			return "", fmt.Errorf("could not parse the output of grok -p: %w", err)
		}
		text := strings.TrimSpace(res.Text)
		if text == "" {
			return "", fmt.Errorf("grok -p returned no text (stop reason %s)", res.StopReason)
		}
		return text, nil
	}
}

// agyArgs is the argument list for one agy --print run. An empty model leaves --model off, which is what keeps the CLI's own default.
func agyArgs(prompt, model string) []string {
	args := []string{"--print", prompt, "--output-format", "json", "--disable-slash-commands"}
	if model != "" {
		args = append(args, "--model", model)
	}
	return args
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

// runCLI runs one child process to completion under a hard timeout and returns its stdout.
// Input: the binary, the timeout in seconds, the arguments, and what to feed the child on stdin. Output: stdout, or an error naming the timeout, the exit status or the stderr the child died with.
func runCLI(ctx context.Context, binary string, timeoutSeconds int, args []string, stdin string) ([]byte, error) {
	timeout := time.Duration(timeoutSeconds) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdin = strings.NewReader(stdin)
	// Both CLIs read project files, per-directory settings and instruction files out of their working directory, and the daemon's working directory is wherever the user happened to launch it from. An empty temporary directory makes one run look like every other run.
	cmd.Dir = os.TempDir()
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	// Killing the child does not kill its own children, and stdout stays open as long as any of them holds it, so without this a hung run would still block here for as long as its grandchildren live. WaitDelay stops the waiting a couple of seconds after the kill instead.
	// ponytail: the timeout stops ORA waiting, it does not guarantee the process tree is gone; kill the whole process group if a hung CLI is ever seen to linger.
	cmd.WaitDelay = 2 * time.Second

	name := filepath.Base(binary)
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("%s timed out after %s", name, timeout)
		}
		slog.Debug("a brain CLI exited with an error", "binary", name, "stderr", head(stderr.String()))
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return out.Bytes(), nil
}

// The CLIs' own output is logged at debug rather than returned in the error, because these errors are logged at warn by callers such as the evening close (slog.Warn("evening close failed", "error", err)) and the output can carry part of the prompt — a day of the user's screen text — or a login error naming their account.

// head is the first 300 runes of s with the whitespace squeezed out, which is as much of a CLI's error output as belongs in one log line. Cutting on runes rather than bytes keeps the log line valid UTF-8 whatever the CLI printed.
func head(s string) string {
	return util.RunesEllipsis(util.OneLine(s), 300)
}
