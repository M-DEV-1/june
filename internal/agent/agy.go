// agy.go answers /ask with Antigravity's `agy` command line on the user's own plan, and offers it Ora's own tools over the same MCP tool server claude.go starts — claudeToolServer is reused as it is, not copied, because it already runs every call through the ask's gate, its step budget and its hop recording.
// agy has no inline MCP flag and no way to allow a subset of tools for one run: its only MCP surface is `agy mcp add`, which would mutate the user's own global config, and its only per-run permission surface is a grant file under its own HOME. So instead of an MCP config on argv, one run gets a whole throwaway HOME: a mirror of the user's real ~/.gemini with everything but its own two config files symlinked through (which is how agy still finds its own login), and Ora's own mcp_config.json and config.json in their place, granting nothing but the tool server. --dangerously-skip-permissions is never passed, because the point of that grant file is that only Ora's tools are pre-approved — anything else must stay ungranted, and in print mode an ungranted tool cannot be interactively approved, which is exactly the refusal wanted for anything outside Ora's own tools.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"ora/internal/util"
)

const (
	// agyBinary is the Antigravity command line, found on PATH.
	agyBinary = "agy"
	// agyAskTimeout bounds one whole ask, every tool round included, matching claudeAskTimeout.
	agyAskTimeout = 12 * time.Minute
	// agyMCPServerName is what Ora calls its own tool server inside the temp HOME an agy run uses, and so is the name a grant of the form mcp(<name>/*) refers to.
	agyMCPServerName = "ora"
)

// ProviderAgy is the Antigravity command line, which serves Gemini models on the user's own plan.
const ProviderAgy = "agy"

// agyModel is the model an ask asks for. Input: none. Output: the model last picked for Antigravity in the window, else ORA_AGY_MODEL, else "" — agy has its own default model when none is named, and nothing here has read a fact establishing what that default id is, so it is left to the CLI rather than guessed.
func agyModel() string {
	return pickedModel("antigravity", "ORA_AGY_MODEL", "")
}

// buildAgyHome builds a throwaway HOME for one agy run at tempHome: it symlinks every entry of the real ~/.gemini into place except the config directory, symlinks every entry of the real ~/.gemini/config except mcp_config.json and config.json, and writes Ora's own versions of those two files, naming only the tool server at mcpURL and granting only its tools. Nothing else the user's real home holds is reachable from the run — no other rules, no other MCP servers, no shell grants — while agy still finds its own login because that lives under an entry this function only symlinks through. Input: the real home directory to mirror from, the fresh temp directory to build the mirror in, and the tool server's URL. Output: an error naming what could not be listed, linked or written.
func buildAgyHome(realHome, tempHome, mcpURL string) error {
	configDir := filepath.Join(tempHome, ".gemini", "config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return fmt.Errorf("agy: making the temp .gemini/config: %w", err)
	}

	realGemini := filepath.Join(realHome, ".gemini")
	if err := symlinkEntries(realGemini, filepath.Join(tempHome, ".gemini"), "config"); err != nil {
		return err
	}
	realConfig := filepath.Join(realGemini, "config")
	if err := symlinkEntries(realConfig, configDir, "mcp_config.json", "config.json"); err != nil {
		return err
	}

	// serverUrl is the key agy itself writes for an http server (agy mcp add, 1.1.27); the {"type":"http","url":...} shape is read as nothing, and the ask then ran with no tools at all (2026-09-09).
	mcpConfig := fmt.Sprintf(`{"mcpServers":{%q:{"serverUrl":%q}}}`, agyMCPServerName, mcpURL)
	if err := os.WriteFile(filepath.Join(configDir, "mcp_config.json"), []byte(mcpConfig), 0o600); err != nil {
		return fmt.Errorf("agy: writing the mcp config: %w", err)
	}
	grants := fmt.Sprintf(`{"userSettings":{"globalPermissionGrants":{"allow":["mcp(%s/*)"]}}}`, agyMCPServerName)
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(grants), 0o600); err != nil {
		return fmt.Errorf("agy: writing the permission grants: %w", err)
	}
	return nil
}

// symlinkEntries symlinks every entry of src into dst, skipping the names in except. Input: the directory to read entries from, the directory to place the symlinks in (already created), and the entry names to leave out. Output: an error naming what could not be read or linked; a src directory that does not exist yet is not an error, since a machine that has never run agy has no ~/.gemini at all.
func symlinkEntries(src, dst string, except ...string) error {
	entries, err := os.ReadDir(src)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("agy: reading %s: %w", src, err)
	}
	skip := make(map[string]bool, len(except))
	for _, name := range except {
		skip[name] = true
	}
	for _, entry := range entries {
		if skip[entry.Name()] {
			continue
		}
		if err := os.Symlink(filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name())); err != nil {
			return fmt.Errorf("agy: linking %s: %w", entry.Name(), err)
		}
	}
	return nil
}

// agyEnv is the environment one agy run gets: the daemon's own environment with HOME replaced by the throwaway mirror, so agy reads its login and Ora's grants from tempHome while everything else about the process (PATH included) stays normal.
func agyEnv(tempHome string) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "HOME=") {
			continue
		}
		env = append(env, e)
	}
	return append(env, "HOME="+tempHome)
}

// agyArgs is the argument list for one `agy --print` run. Input: the model to ask for ("" leaves --model off, keeping the CLI's own default) and the whole prompt, instruction included since agy has no system-prompt flag of its own. Output: the arguments. --dangerously-skip-permissions is deliberately never included here — see the package comment.
func agyArgs(model, prompt string) []string {
	args := []string{"--print", prompt, "--output-format", "json", "--disable-slash-commands", "--print-timeout", agyAskTimeout.String()}
	if model != "" {
		args = append(args, "--model", model)
	}
	return args
}

// agyResult is the object `agy --print --output-format json` prints when the run is over.
type agyResult struct {
	Status   string `json:"status"`
	Response string `json:"response"`
	// NumTurns is how many rounds the CLI took, and Usage what they cost. Both are reported by agy and were being thrown away, so every Antigravity answer showed in the usage table as a row of zeroes.
	NumTurns int `json:"num_turns"`
	Usage    struct {
		InputTokens     int `json:"input_tokens"`
		OutputTokens    int `json:"output_tokens"`
		ThinkingTokens  int `json:"thinking_tokens"`
		CacheReadTokens int `json:"cache_read_tokens"`
		TotalTokens     int `json:"total_tokens"`
	} `json:"usage"`
}

// agyRunner runs one CLI process to completion. Input: the environment to run it under (HOME pointed at the throwaway mirror) and the arguments. Output: its stdout, or an error naming what went wrong. A test replaces it with a stub so a whole ask, tool calls included, runs without the CLI.
type agyRunner func(ctx context.Context, env []string, args []string) ([]byte, error)

// runAgyCLI is the runner that actually starts the command line. The working directory is an empty temporary one, so the CLI finds no project files of the user's to read, matching runClaudeCLI.
func runAgyCLI(binary string) agyRunner {
	return func(ctx context.Context, env []string, args []string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Env = env
		cmd.Dir = os.TempDir()
		var out, stderr bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &stderr
		// Killing the child does not kill its own children, and stdout stays open as long as any of them holds it, so without this a hung run would block here for as long as its grandchildren live.
		cmd.WaitDelay = 2 * time.Second
		if err := cmd.Run(); err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, fmt.Errorf("agy: the run timed out after %s", agyAskTimeout)
			}
			// A run that failed still prints its result object on stdout, and that object says more about why than the exit status does, so it is preferred when it is there.
			if len(out.Bytes()) > 0 {
				return out.Bytes(), nil
			}
			return nil, fmt.Errorf("agy: %w: %s", err, agyHead(stderr.String()))
		}
		return out.Bytes(), nil
	}
}

// agyHead is the first 300 runes of s with the whitespace squeezed out, which is as much of a CLI's error output as belongs in one log line.
func agyHead(s string) string {
	return util.RunesEllipsis(util.OneLine(s), 300)
}

// AskAgy answers a question through the Antigravity command line on the user's own plan, running Ora's tools through the same gate and trace as every other ask. Output: the turn trace with the answer, tool hops, evidence and model "agy/<model>" (or "agy" when no model was named), or the trace so far and an error.
func (a *Agent) AskAgy(ctx context.Context, question string) (TurnTrace, error) {
	return a.AskAgyWith(ctx, nil, question)
}

// AskAgyWith is AskAgy with the conversation so far sent ahead of the question, so a follow-up reads as one. Input: the prior turns (see HistoryFromTurns), nil for a question that stands alone, and the question. Output: the same TurnTrace AskAgy returns.
func (a *Agent) AskAgyWith(ctx context.Context, history History, question string) (TurnTrace, error) {
	return a.askAgy(ctx, runAgyCLI(agyBinary), agyModel(), history, question)
}

// askAgy is AskAgyWith against the given runner and model, so a test can drive a whole ask without the command line.
func (a *Agent) askAgy(ctx context.Context, run agyRunner, model string, history History, question string) (TurnTrace, error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, agyAskTimeout)
	defer cancel()
	// The look allowance, the picture draw maps coordinates against, and what the pictures cost all belong to one ask, carried on ctx from here on so a concurrent ask never shares this one's screenshot.
	ctx = withAskLookState(ctx)
	instruction, handshake := a.HandshakePrompt(ctx, start)

	recallCtx, cancelRecall := context.WithTimeout(ctx, textSendLoopRetrieveTimeout)
	injected, err := a.brain.RetrieveRelevant(recallCtx, question, 2)
	cancelRecall()
	if err != nil {
		slog.Warn("AskAgy retrieve relevant failed, continuing without inject", "error", err)
		injected = nil
	}

	modelLabel := ProviderAgy
	if model != "" {
		modelLabel = ProviderAgy + "/" + model
	}
	tr := TurnTrace{
		Channel:   ChannelText,
		Model:     modelLabel,
		Question:  question,
		Handshake: handshake,
		Injected:  injected,
		Usage:     TokenUsage{Provider: ProviderAgy},
	}

	// claudeToolServer is reused as it is: it already runs every call through the ask's own gate, step budget and hop recording, and does not care which CLI is on the other end of the HTTP connection.
	server, err := a.startClaudeToolServer(ctx)
	if err != nil {
		tr.Duration = time.Since(start)
		return tr, err
	}
	defer server.Close()

	// agy has no system-prompt flag, so Ora's instruction goes at the head of the prompt text instead of in a file of its own. After it, the same order askClaude sends: the thread, then this turn's time and recalled memory, then the reference to a close past run, then the question last.
	var prompt strings.Builder
	prompt.WriteString(instruction + "\n\n")
	if thread := claudeThread(history); thread != "" {
		prompt.WriteString(thread + "\n")
	}
	prompt.WriteString(turnContext(start, injected) + "\n\n")
	if reference := a.ActReferenceFor(ctx, question, start); reference != "" {
		prompt.WriteString(reference + "\n\n")
	}
	prompt.WriteString(question)

	realHome, err := os.UserHomeDir()
	if err != nil {
		tr.Duration = time.Since(start)
		return tr, fmt.Errorf("agy: finding the real home directory to mirror: %w", err)
	}
	tempHome, err := os.MkdirTemp(os.TempDir(), "ora-agy-ask-")
	if err != nil {
		tr.Duration = time.Since(start)
		return tr, fmt.Errorf("agy: making the ask's own temp home: %w", err)
	}
	defer os.RemoveAll(tempHome)
	if err := buildAgyHome(realHome, tempHome, server.URL()); err != nil {
		tr.Duration = time.Since(start)
		return tr, err
	}

	out, err := run(ctx, agyEnv(tempHome), agyArgs(model, prompt.String()))
	tr.ToolHops = server.Hops()
	tr.Evidence = evidenceFromToolHops(tr.ToolHops)
	tr.ImageTokens = lookTokensSpent(ctx)
	tr.Duration = time.Since(start)
	if err != nil {
		return tr, err
	}
	var res agyResult
	if err := json.Unmarshal(out, &res); err != nil {
		return tr, fmt.Errorf("agy: could not parse what the command line printed: %w (%s)", err, agyHead(string(out)))
	}
	if res.Status != "SUCCESS" {
		return tr, fmt.Errorf("agy: the run failed (%s): %s", res.Status, agyHead(res.Response))
	}
	tr.Answer = strings.TrimSpace(res.Response)
	// What the run cost, recorded the same way the Claude asker records its own: what the model read afresh plus what it answered out of its cache is all input it read, so the whole input is their sum and the cached part is one of them. Thinking is not added to the output: measured against the real command line on 2026-09-07, a one-round ask reported output_tokens 35 and thinking_tokens 26 for a nine-token reply, so the thinking is already inside the output count and adding it would bill it twice. agy's own total_tokens is ignored for the same reason the Claude path ignores its input_tokens alone: it leaves the cache read out, so that same ask would go on record as 6,471 tokens rather than 14,598.
	tr.Usage.Rounds = res.NumTurns
	input := res.Usage.InputTokens + res.Usage.CacheReadTokens
	output := res.Usage.OutputTokens
	tr.Usage.add(input, output, input+output)
	tr.Usage.CachedInputTokens = res.Usage.CacheReadTokens
	slog.Debug("ask agy: done", "model", model, "turns", res.NumTurns, "tools", len(tr.ToolHops), "input_tokens", res.Usage.InputTokens, "output_tokens", res.Usage.OutputTokens, "cached_input_tokens", res.Usage.CacheReadTokens, "duration", tr.Duration)
	if tr.Answer == "" {
		return tr, errors.New("agy: the run returned no text")
	}
	return tr, nil
}

// AgyBrain is the asker the daemon registers under the "agy" brain name; it answers through the agent's AskAgy.
type AgyBrain struct {
	Agent *Agent
}

// AskText answers the question through AskAgy, so the ipc server can route an "agy" ask like any other brain.
func (b AgyBrain) AskText(ctx context.Context, question string) (TurnTrace, error) {
	return b.Agent.AskAgy(ctx, question)
}

// AskTextWith answers the question with the conversation so far, so the ipc server can hand an "agy" ask its thread exactly as it hands one to the default brain. Input: the prior turns and the question. Output: the turn trace from AskAgyWith.
func (b AgyBrain) AskTextWith(ctx context.Context, history History, question string) (TurnTrace, error) {
	return b.Agent.AskAgyWith(ctx, history, question)
}
