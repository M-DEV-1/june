// agy.go answers /ask with Antigravity's `agy` command line on the user's own plan, and offers it Ora's own tools over the same MCP tool server claude.go starts — claudeToolServer is reused as it is, not copied, because it already runs every call through the ask's gate, its step budget and its hop recording.
// agy has no inline MCP flag and no way to allow a subset of tools for one run: its only MCP surface is `agy mcp add`, which would mutate the user's own global config, and its only per-run permission surface is a grant file under its own HOME. So instead of an MCP config on argv, one run gets a whole throwaway HOME: a mirror of the user's real ~/.gemini with everything but its own two config files symlinked through (which is how agy still finds its own login), and Ora's own mcp_config.json and config.json in their place, granting nothing but the tool server. --dangerously-skip-permissions is never passed, because the point of that grant file is that only Ora's tools are pre-approved — anything else must stay ungranted, and in print mode an ungranted tool cannot be interactively approved, which is exactly the refusal wanted for anything outside Ora's own tools.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
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
	if err := symlinkEntries(realGemini, filepath.Join(tempHome, ".gemini"), "config", "antigravity-cli"); err != nil {
		return err
	}
	if err := buildAgyCLIDir(realGemini, tempHome); err != nil {
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

// buildAgyCLIDir mirrors the user's ~/.gemini/antigravity-cli into the run's temp HOME with one file of Ora's own in it: settings.json, carrying every setting the user already had plus Ora's statusline command. That command is the only way to read the plan's own allowance on a machine Ora cannot take the OAuth token from — agy pipes its quota to whatever statusline it is given, on a --print run as much as an interactive one, and reports it nowhere else. The user's real settings.json is never touched. Input: the real ~/.gemini and the temp HOME. Output: an error naming what could not be listed, linked or written.
func buildAgyCLIDir(realGemini, tempHome string) error {
	tempCLI := filepath.Join(tempHome, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(tempCLI, 0o700); err != nil {
		return fmt.Errorf("agy: making the temp antigravity-cli: %w", err)
	}
	realCLI := filepath.Join(realGemini, "antigravity-cli")
	if err := symlinkEntries(realCLI, tempCLI, "settings.json"); err != nil {
		return err
	}
	// Read rather than replaced, so a run keeps the user's theme, model default and everything else. A settings.json that is missing or unreadable is not an error: a machine that has never run agy has none, and the run only needs the statusline.
	settings := map[string]any{}
	if data, err := os.ReadFile(filepath.Join(realCLI, "settings.json")); err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			slog.Warn("agy: the user's settings.json is not JSON, the run gets Ora's statusline alone", "error", err)
			settings = map[string]any{}
		}
	}
	settings["statusLine"] = map[string]any{"command": agyStatusLine(tempHome)}
	data, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("agy: building the run's settings: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tempCLI, "settings.json"), data, 0o600); err != nil {
		return fmt.Errorf("agy: writing the run's settings: %w", err)
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

// agyResult is the object agy's "result" event carries when a turn is over.
type agyResult struct {
	Status   string `json:"status"`
	Response string `json:"response"`
	// Error is where agy puts the reason a run failed, and on a failure Response is empty. Reading only Response reported every failure as "agy: the run failed (ERROR):" with nothing after the colon (2026-09-15).
	Error string `json:"error"`
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

// agyFailure is the error one failed agy run becomes. Input: the run's result. Output: an error naming the status and the reason agy gave, wrapping ErrLoggedOut when that reason is an expired login, which is what tells the router to hand the question to the next brain instead of failing the ask.
func agyFailure(res agyResult) error {
	reason := res.Error
	if reason == "" {
		reason = res.Response
	}
	err := fmt.Errorf("agy: the run failed (%s): %s", res.Status, agyHead(reason))
	if loggedOut(reason) {
		return fmt.Errorf("%w: %w", ErrLoggedOut, err)
	}
	return err
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
	return a.askAgy(ctx, newAgyProcess, agyModel(), history, question)
}

// askAgy is AskAgyWith against the given session constructor and model, so a test can drive a whole ask without the command line. newProc is called only when a fresh process has to be started — reusing the agent's live one, when there is one for this model, never calls it again.
func (a *Agent) askAgy(ctx context.Context, newProc func() agySessionRunner, model string, history History, question string) (TurnTrace, error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, agyAskTimeout)
	defer cancel()
	// The look allowance, the picture draw maps coordinates against, and what the pictures cost all belong to one ask, carried on ctx from here on so a concurrent ask never shares this one's screenshot.
	ctx = withAskLookState(ctx)
	instruction := a.LeanPrompt(start)
	var handshake []string

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

	reference, shownLessons := a.actReference(ctx, question, start)
	tr.LessonsShown = shownLessons
	res, hops, capped, err := a.runAgyTurn(ctx, newProc, model, instruction, history, start, injected, reference, question)
	tr.ToolHops = hops
	tr.Evidence = evidenceFromToolHops(hops)
	tr.ImageTokens = lookTokensSpent(ctx)
	tr.Duration = time.Since(start)
	if capped {
		return tr, capError(hops)
	}
	if err != nil {
		return tr, err
	}
	if res.Status != "SUCCESS" {
		return tr, agyFailure(res)
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

// agyBrainID is the id GET /brains publishes this provider under, which is also the key its usage reading is stored against. It differs from ProviderAgy ("agy", the CLI) because the picker names the plan rather than the command line.
const agyBrainID = "antigravity"

// recordAgyQuota reads the plan allowance Ora's statusline command captured during a run and records it against the Antigravity brain row. Input: the run's throwaway HOME. Output: none — a run whose payload is missing or carries no quota records nothing, which leaves the last good reading on screen rather than replacing it with empty bars.
func recordAgyQuota(tempHome string) {
	payload, err := os.ReadFile(filepath.Join(tempHome, agyQuotaFile))
	if err != nil {
		return
	}
	if limits := agyQuotaLimits(payload); len(limits) > 0 {
		recordUsage(agyBrainID, limits)
	}
}

// agyQuotaFile is the name, inside one run's throwaway HOME, that Ora's own statusline command writes agy's payload to.
const agyQuotaFile = "ora-quota.json"

// agyStatusLine is the statusline command Ora gives agy in the throwaway HOME: it copies the payload agy pipes in to agyQuotaFile and prints one word, because agy renders whatever the command prints. Input: the temp HOME. Output: the shell command line, with the path quoted so a temp directory with a space in it still works.
func agyStatusLine(tempHome string) string {
	return "cat > " + shellQuote(filepath.Join(tempHome, agyQuotaFile))
}

// shellQuote wraps s so a shell reads it as one literal word. Input: any path. Output: the single-quoted form, with each embedded quote closed, escaped and reopened. Go's %q is not this: it is Go string syntax, and a path holding a dollar sign or a backslash — TMPDIR is the user's to set — would come out as something the shell expands rather than the path Ora meant, with no error anywhere and no quota captured.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// agyQuotaLimits reads the plan allowance out of the payload agy pipes to its statusline command. Input: the payload as agy wrote it. Output: one window per entry of its "quota" object, or nothing at all when the payload is not JSON or carries no quota — an empty result must leave the last good reading alone rather than record empty bars.
// agy reports how much of a window is left and Ora draws how much is spent, so the fractions are inverted here. The window names are agy's own ("gemini-5h", "3p-weekly"): the plan meters the Gemini models and the third-party ones it also carries separately, and collapsing them would hide a family that is spent behind one that is not.
func agyQuotaLimits(payload []byte) []UsageLimit {
	var doc struct {
		Quota map[string]struct {
			RemainingFraction float64 `json:"remaining_fraction"`
			ResetTime         string  `json:"reset_time"`
		} `json:"quota"`
	}
	if err := json.Unmarshal(payload, &doc); err != nil {
		return nil
	}
	limits := make([]UsageLimit, 0, len(doc.Quota))
	for _, window := range slices.Sorted(maps.Keys(doc.Quota)) {
		q := doc.Quota[window]
		resets, _ := time.Parse(time.RFC3339, q.ResetTime)
		limits = append(limits, UsageLimit{
			Window:       window,
			UsedFraction: 1 - q.RemainingFraction,
			ResetsAt:     resets,
			Source:       "agy statusline quota." + window,
		})
	}
	return limits
}
