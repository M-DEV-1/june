package brain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ClaudeCLI answers by running `claude -p`, which uses whatever Claude Code login the machine already has — a subscription, billed as a subscription, not per-token API calls.
// --bare is deliberately never passed: it makes the CLI read ANTHROPIC_API_KEY instead of the login, which is the billing this whole path exists to avoid.
// --restricted and --strict-mcp-config are always passed, because the prompt carries text nobody vetted (a meeting transcript, whatever was on the user's screens) and this duty needs no tools at all; a run that cannot open a shell or a file cannot be talked into one.
// The prompt goes in on stdin: a single argv entry is capped at 128 KB on Linux and a long meeting is bigger than that.
// Input: the path to the binary and a hard timeout in seconds. Output: the "result" field of the CLI's JSON.
func ClaudeCLI(binary string, timeoutSeconds int) Brain {
	return func(ctx context.Context, prompt string) (string, error) {
		out, err := runCLI(ctx, binary, timeoutSeconds, []string{"-p", "--output-format", "json", "--restricted", "--strict-mcp-config"}, prompt)
		if err != nil {
			return "", err
		}
		var res struct {
			Result  string `json:"result"`
			IsError bool   `json:"is_error"`
			Subtype string `json:"subtype"`
		}
		if err := json.Unmarshal(out, &res); err != nil {
			return "", fmt.Errorf("could not parse the output of claude -p: %w (%s)", err, head(string(out)))
		}
		if res.IsError {
			return "", fmt.Errorf("claude -p failed (%s): %s", res.Subtype, head(res.Result))
		}
		text := strings.TrimSpace(res.Result)
		if text == "" {
			return "", fmt.Errorf("claude -p returned no text")
		}
		return text, nil
	}
}

// AgyCLI answers by running Antigravity's `agy --print`, under the Antigravity login the machine already has.
// The prompt is the value of --print rather than stdin, which is the only text input that CLI takes; that caps a prompt at one argv entry, 128 KB on Linux.
// The timeout matters more here than for claude: agy has open bugs where a print run with no terminal attached never returns, and a probe of a one-word prompt took 29 seconds of wall clock for 13 seconds of model time.
// Input: the path to the binary and a hard timeout in seconds. Output: the "response" field of the CLI's JSON.
func AgyCLI(binary string, timeoutSeconds int) Brain {
	return func(ctx context.Context, prompt string) (string, error) {
		out, err := runCLI(ctx, binary, timeoutSeconds, []string{"--print", prompt, "--output-format", "json"}, "")
		if err != nil {
			return "", err
		}
		var res struct {
			Status   string `json:"status"`
			Response string `json:"response"`
		}
		if err := json.Unmarshal(out, &res); err != nil {
			return "", fmt.Errorf("could not parse the output of agy --print: %w (%s)", err, head(string(out)))
		}
		if res.Status != "SUCCESS" {
			return "", fmt.Errorf("agy --print failed with status %s: %s", res.Status, head(res.Response))
		}
		text := strings.TrimSpace(res.Response)
		if text == "" {
			return "", fmt.Errorf("agy --print returned no text")
		}
		return text, nil
	}
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
		return nil, fmt.Errorf("%s: %w: %s", name, err, head(stderr.String()))
	}
	return out.Bytes(), nil
}

// head is the first 300 characters of s with the whitespace squeezed out, which is as much of a CLI's error output as belongs in one log line.
func head(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
