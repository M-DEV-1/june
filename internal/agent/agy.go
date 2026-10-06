// agy.go answers /ask with Antigravity's `agy` command line on the user's own plan, and offers it June's own tools over the same MCP tool server claude.go starts — claudeToolServer is reused as it is, not copied, because it already runs every call through the ask's gate, its step budget and its hop recording.
// agy has no inline MCP flag and no way to allow a subset of tools for one run: its only MCP surface is `agy mcp add`, which would mutate the user's own global config, and its only per-run permission surface is a grant file under its own HOME. So instead of an MCP config on argv, one run gets a whole throwaway HOME: a mirror of the user's real ~/.gemini with every directory linked through whole (which is how agy keeps its own conversations and state where the user's own runs find them; its login is in the OS keyring, not under HOME), and June's own mcp_config.json and config.json in place of the user's, naming only the tool server and granting only its tools. --dangerously-skip-permissions is never passed, because the point of that grant file is that only June's tools are pre-approved.
// Anything else is refused by name rather than left ungranted. An ungranted tool cannot be approved in print mode either, but agy soft-denies it by ending the whole turn with SUCCESS and an empty response, which is how a screen question that made the model reach for run_command came back as "agy: the run returned no text" (2026-10-03). A deny rule instead hands the model a tool error and the turn goes on, measured against agy 1.2.15 on 2026-10-03: "Permission denied for command(echo probe-ok). Matches user-configured deny rule.", then an answer.
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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"june/internal/util"
)

const (
	// agyBinary is the Antigravity command line, found on PATH.
	agyBinary = "agy"
	// agyAskTimeout bounds one whole ask, every tool round included, matching claudeAskTimeout.
	agyAskTimeout = 12 * time.Minute
	// agyMCPServerName is what June calls its own tool server inside the temp HOME an agy run uses, and so is the name a grant of the form mcp(<name>/*) refers to.
	agyMCPServerName = "june"
	// agySessionHomePrefix and agyDutyHomePrefix name the throwaway HOMEs in the temp directory, so the sweep at daemon start can find the ones a killed daemon left behind and touch nothing else.
	agySessionHomePrefix = "june-agy-session-"
	agyDutyHomePrefix    = "june-agy-duty-"
	// agyWorkspaceDir is the empty directory inside a throwaway HOME that agy runs in. agy takes its working directory as its workspace, and the temp directory it ran in before held every other run's HOME and whatever else the machine leaves there.
	agyWorkspaceDir = "work"
)

// ProviderAgy is the Antigravity command line, which serves Gemini models on the user's own plan.
const ProviderAgy = "agy"

// agyModel is the model an ask asks for. Input: none. Output: the model last picked for Antigravity in the window, else JUNE_AGY_MODEL, else "" — agy has its own default model when none is named, and nothing here has read a fact establishing what that default id is, so it is left to the CLI rather than guessed.
func agyModel() string {
	return pickedModel("antigravity", "JUNE_AGY_MODEL", "")
}

// agyAskDenied is every kind of agy's own tool an ask refuses outright, in agy's grant grammar: running a command, reading or writing a file, and fetching or driving a web page. That leaves an ask what the Claude path has: June's tools and the CLI's own web search. Reading a file is refused as well, because a model sent to read the screen went looking for the file on disk instead (2026-10-03), and a file read beside a web search is a way out for whatever a page on screen tells the model to fetch. search_web is not in any of these kinds and cannot be refused this way (checked 2026-10-03: it ran under a deny list holding all of them).
var agyAskDenied = []string{"command(*)", "read_file(*)", "write_file(*)", "read_url(*)", "execute_url(*)"}

// agyDutyDenied is the same for a duty, which needs no tool at all and whose prompt carries text nobody vetted, so every MCP server is refused too. An ask cannot refuse mcp(*): it would refuse June's own server along with the rest. A duty runs as AgyDutyAgent, which is offered no tool at all, so this list is what still stands if a later agy stops finding that agent: an --agent it cannot find runs the default agent without a word (measured on agy 1.2.16, 2026-10-03).
var agyDutyDenied = append(slices.Clone(agyAskDenied), "mcp(*)")

// AgyDutyAgent is the custom agent an agy duty and the /usage read run as (`--agent`), defined by agyDutyAgentFile in the run's own workspace. A deny rule refuses a tool only once it is called, so under the default agent a duty was still sent agy's whole tool roster, about 15,700 tokens of it, and could still reach search_web, which no deny rule refuses, with screen and meeting text in its prompt. This agent opts out of agy's default prompt sections and built-in tools and lists none of its own: measured on agy 1.2.16 on 2026-10-03, the same one-line duty read 583 input tokens instead of 15,683, and the model named manage_task (its own background-task list) as the only tool it had.
const AgyDutyAgent = "june-duty"

// agyDutyAgentFile is AgyDutyAgent's definition, in agy's Markdown custom-agent format: YAML frontmatter, then the system prompt under an H1. inheritCustomizations and inheritMcp are off so that the user's own rules, skills and MCP servers stay out of June's duties. It goes in the workspace's .agents/agents, where agy looks for a project's custom agents in a headless run.
const agyDutyAgentFile = `---
name: june-duty
description: Answers one request from June in text, with no tools.
mainAgent: true
subagent: false
excludeDefaultComponents: true
inheritMcp: false
inheritCustomizations: false
tools: []
---
# June duty

Answer the request you are sent with text alone. You have no tools. When the request asks you to name a tool or an action, or to reply in JSON, write that out as your text.
`

// agyToolNote is told to the model on a session's first turn, so it does not spend a round discovering that its own tools are refused. June's branch tool is not named: an ask is not offered it.
const agyToolNote = "[tools] Only the tools June serves you, on the june MCP server, work here, along with your own web search. Your own tools for running commands, reading or writing files and opening web pages are refused, so do not reach for them: use June's tools for the screen and the user's memory, and your web search for anything you need to look up."

// buildAgyHome builds a throwaway HOME for one agy run at tempHome: every entry of the real ~/.gemini is linked into place except the config directory (see linkEntry for what a link is on each system), every entry of the real ~/.gemini/config is linked except mcp_config.json and config.json, and June's own versions of those two files are written: the tool server at mcpURL as the only MCP server and its tools as the only grant, or for a duty (mcpURL "") no server and no grant at all. An empty workspace directory is made beside them. Input: the real home directory to mirror from, the fresh temp directory to build the mirror in, and the tool server's URL. Output: an error naming what could not be listed, linked or written.
func buildAgyHome(realHome, tempHome, mcpURL string) error {
	configDir := filepath.Join(tempHome, ".gemini", "config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return fmt.Errorf("agy: making the temp .gemini/config: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(tempHome, agyWorkspaceDir), 0o700); err != nil {
		return fmt.Errorf("agy: making the run's workspace: %w", err)
	}

	// antigravity-cli is linked whole with the rest: it holds agy's SQLite stores, and only a directory link keeps a database's -wal and -shm beside the real file. June used to make it a directory of its own to put a statusline command in it, which agy never runs in print mode (see readAgyUsage).
	realGemini := filepath.Join(realHome, ".gemini")
	if err := symlinkEntries(realGemini, filepath.Join(tempHome, ".gemini"), "config"); err != nil {
		return err
	}
	realConfig := filepath.Join(realGemini, "config")
	if err := symlinkEntries(realConfig, configDir, "mcp_config.json", "config.json"); err != nil {
		return err
	}

	servers := map[string]any{}
	allow := []string{}
	deny := agyDutyDenied
	if mcpURL != "" {
		// serverUrl is the key agy itself writes for an http server (agy mcp add, 1.1.27); the {"type":"http","url":...} shape is read as nothing, and the ask then ran with no tools at all (2026-09-09).
		servers[agyMCPServerName] = map[string]any{"serverUrl": mcpURL}
		allow = []string{"mcp(" + agyMCPServerName + "/*)"}
		deny = agyAskDenied
	}
	// Maps of plain strings and string slices always marshal; the errors are impossible to hit.
	mcpConfig, _ := json.Marshal(map[string]any{"mcpServers": servers})
	if err := os.WriteFile(filepath.Join(configDir, "mcp_config.json"), mcpConfig, 0o600); err != nil {
		return fmt.Errorf("agy: writing the mcp config: %w", err)
	}
	grants, _ := json.Marshal(map[string]any{"userSettings": map[string]any{"globalPermissionGrants": map[string]any{"allow": allow, "deny": deny}}})
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), grants, 0o600); err != nil {
		return fmt.Errorf("agy: writing the permission grants: %w", err)
	}
	return nil
}

// sqliteSidecar reports whether name is a SQLite journal file. One is never linked or copied on its own: SQLite finds it by the database's own path, so a copy beside a linked or copied database is either ignored or, worse, replayed against a database it was never written for.
func sqliteSidecar(name string) bool {
	return strings.HasSuffix(name, "-wal") || strings.HasSuffix(name, "-shm") || strings.HasSuffix(name, "-journal")
}

// symlinkEntries links every entry of src into dst (a symlink, or on a Windows desk without the symlink privilege a junction for a directory and a copy for a file — see linkEntry), skipping the names in except and any SQLite journal file. Input: the directory to read entries from, the directory to place the links in (already created), and the entry names to leave out. Output: an error naming what could not be read or linked; a src directory that does not exist yet is not an error, since a machine that has never run agy has no ~/.gemini at all.
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
		if skip[entry.Name()] || sqliteSidecar(entry.Name()) {
			continue
		}
		if err := linkEntry(filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name())); err != nil {
			return fmt.Errorf("agy: linking %s: %w", entry.Name(), err)
		}
	}
	return nil
}

// agyEnv is the environment one agy run gets: the daemon's own environment with HOME replaced by the throwaway mirror, so agy reads its login and June's grants from tempHome while everything else about the process (PATH included) stays normal. USERPROFILE is replaced too, because agy is a Go program and Go's home directory on Windows is USERPROFILE, not HOME — with HOME alone the run read the user's real ~/.gemini and never saw June's tool server. Windows matches variable names without regard to case, so the old values are dropped the same way.
func agyEnv(tempHome string) []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, e := range os.Environ() {
		name, _, _ := strings.Cut(e, "=")
		if strings.EqualFold(name, "HOME") || strings.EqualFold(name, "USERPROFILE") {
			continue
		}
		env = append(env, e)
	}
	// agy checks for its own updates on start and, when the last check is stale, launches a detached --bg-updater whose children open a visible console: on Windows that flashed a Terminal window and took the focus after June's asks (2026-10-03). Each run here has a throwaway HOME, so agy always thought its last check was stale. The user's own agy runs still update it.
	return append(env, "HOME="+tempHome, "USERPROFILE="+tempHome, "AGY_CLI_DISABLE_AUTO_UPDATE=1")
}

// agyHomeRemoveTries is how many times a throwaway HOME's removal is tried, with the wait between tries doubling from half a second. A file the dying agy still has mapped cannot be deleted on Windows until the process is gone, and killing its Job Object does not wait for that, so a single RemoveAll right after the kill left the SQLite -shm and its directories behind on most runs (twenty of them by 05:29 on 2026-10-03).
const agyHomeRemoveTries = 5

// removeAgyHome removes one throwaway HOME, retrying while something still holds a file in it. Input: the directory. Output: none — a HOME that will not go is logged, and the sweep at the next daemon start tries again. Only the links are removed, never what they point at: Go's RemoveAll does not follow a symlink or a junction.
func removeAgyHome(dir string) {
	wait := 500 * time.Millisecond
	var err error
	for try := 0; try < agyHomeRemoveTries; try++ {
		if err = os.RemoveAll(dir); err == nil {
			return
		}
		time.Sleep(wait)
		wait *= 2
	}
	slog.Warn("agy: could not remove a run's throwaway home; the next daemon start sweeps it", "dir", dir, "error", err)
}

// agyHomeSweepAge is how long a throwaway HOME must have gone untouched before the sweep takes it for dead. A live session's HOME is touched on every turn and killed after agyIdleTimeout unused, a turn lasts at most agyAskTimeout and a duty at most its own timeout, so two hours leaves no doubt while still clearing a crashed daemon's leftovers the same day.
const agyHomeSweepAge = 2 * time.Hour

// SweepAgyHomes removes the throwaway HOMEs that earlier runs left in the temp directory: a daemon killed without its shutdown, or a removal that kept failing. Input: none. Output: none — what was removed and what would not go are logged. Meant for the daemon's start, before it has any agy run of its own.
func SweepAgyHomes() {
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		slog.Warn("agy: could not list the temp directory to sweep old run homes", "error", err)
		return
	}
	cutoff := time.Now().Add(-agyHomeSweepAge)
	removed := 0
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || !(strings.HasPrefix(name, agySessionHomePrefix) || strings.HasPrefix(name, agyDutyHomePrefix)) {
			continue
		}
		// Credential copies go whatever the HOME's age: a young HOME may still be a live run's, but no run reads them (see linkEntry), and a HOME an earlier June built on Windows carries copies of the Gemini CLI's login that nothing else would ever remove.
		scrubAgyCredentials(filepath.Join(os.TempDir(), name, ".gemini"))
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(os.TempDir(), name)); err != nil {
			slog.Warn("agy: could not sweep an old run home", "dir", name, "error", err)
			continue
		}
		removed++
	}
	if removed > 0 {
		slog.Info("agy: swept old run homes", "removed", removed)
	}
}

// agyCredentialFile reports whether a file at the top of ~/.gemini holds a login or a key: the Gemini CLI's OAuth tokens and account list, and the .env it reads an API key from. A throwaway HOME never gets a copy of one (see linkEntry). Input: the file's name. Output: true for those, and for any name that says it holds a credential, token or secret.
func agyCredentialFile(name string) bool {
	n := strings.ToLower(name)
	switch {
	case n == ".env" || strings.HasPrefix(n, ".env."), n == "google_accounts.json":
		return true
	}
	return strings.Contains(n, "cred") || strings.Contains(n, "token") || strings.Contains(n, "secret")
}

// scrubAgyCredentials removes the credential files copied into one throwaway HOME's .gemini. Only regular files are removed: a symlink there is the real file under a second name, and removing the link would leave the file alone anyway, but nothing here goes near what a link points at. Input: the HOME's .gemini directory. Output: none — a file that will not go is logged.
func scrubAgyCredentials(gemini string) {
	entries, err := os.ReadDir(gemini)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || !agyCredentialFile(e.Name()) {
			continue
		}
		if err := os.Remove(filepath.Join(gemini, e.Name())); err != nil {
			slog.Warn("agy: could not remove a credential copy from a run home", "file", filepath.Join(gemini, e.Name()), "error", err)
		}
	}
}

// AgyDutyHome builds a throwaway HOME for one agy duty run: the same mirror an ask gets, but with no MCP server, a grant file that refuses every kind of tool agy can refuse, and AgyDutyAgent defined in the workspace, because a duty needs no tool and its prompt carries text nobody vetted — under the user's real HOME a duty was offered about sixty tools, including run_command and the browser, with the user's own plugins on top (2026-10-03). The caller passes `--agent` AgyDutyAgent. Output: the environment to run agy under, the workspace directory to run it in (empty but for the agent's definition), a func that removes the HOME once the run is over (at once when nothing holds it, otherwise in the background, so a duty's answer never waits on the retries), and an error naming what could not be built.
func AgyDutyHome() (env []string, dir string, remove func(), err error) {
	realHome, err := os.UserHomeDir()
	if err != nil {
		return nil, "", nil, fmt.Errorf("agy: finding the real home directory to mirror: %w", err)
	}
	tempHome, err := os.MkdirTemp(os.TempDir(), agyDutyHomePrefix)
	if err != nil {
		return nil, "", nil, fmt.Errorf("agy: making the duty's own temp home: %w", err)
	}
	if err := buildAgyHome(realHome, tempHome, ""); err != nil {
		removeAgyHome(tempHome)
		return nil, "", nil, err
	}
	dir = filepath.Join(tempHome, agyWorkspaceDir)
	agents := filepath.Join(dir, ".agents", "agents")
	if err := os.MkdirAll(agents, 0o700); err != nil {
		removeAgyHome(tempHome)
		return nil, "", nil, fmt.Errorf("agy: making the duty's agent directory: %w", err)
	}
	if err := os.WriteFile(filepath.Join(agents, AgyDutyAgent+".md"), []byte(agyDutyAgentFile), 0o600); err != nil {
		removeAgyHome(tempHome)
		return nil, "", nil, fmt.Errorf("agy: writing the duty's agent: %w", err)
	}
	remove = func() {
		if os.RemoveAll(tempHome) != nil {
			go removeAgyHome(tempHome)
		}
	}
	return agyEnv(tempHome), dir, remove, nil
}

// AgyLoginExpired reports whether the reason an agy run gave for failing is its own login having expired, for internal/brain's duty path to wrap ErrLoggedOut the same way an ask does. Input: the run's error text. Output: see loggedOut.
func AgyLoginExpired(reason string) bool { return loggedOut(reason) }

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
	// DeniedActions names the tools agy refused without asking anybody, which is the only trace a soft-denied tool leaves: the turn itself ends SUCCESS with an empty response.
	DeniedActions []struct {
		Action      string `json:"action"`
		DisplayName string `json:"display_name"`
	} `json:"denied_actions"`
}

// deniedTools is the names of the tools a run was refused, once each and in the order agy listed them. Output: "" when it was refused none.
func (r agyResult) deniedTools() string {
	var names []string
	for _, d := range r.DeniedActions {
		name := d.DisplayName
		if name == "" {
			name = d.Action
		}
		if name != "" && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return strings.Join(names, ", ")
}

// agyNudge is the line a session is sent when a turn ended SUCCESS with nothing said, which is what agy does when the model reached for a tool it may not use. Input: the refused tools, "" when agy named none. Output: the follow-up turn's text.
func agyNudge(denied string) string {
	why := ""
	if denied != "" {
		why = ", because " + denied + " is refused here"
	}
	return "Your last turn ended without an answer" + why + ". Answer the question now, in text, using only June's tools and what you already have."
}

// agyFailure is the error one failed agy run becomes. Input: the run's result. Output: an error naming the status and the reason agy gave, wrapping ErrLoggedOut when that reason is an expired login, which is what tells the router to hand the question to the next brain instead of failing the ask.
func agyFailure(res agyResult) error {
	reason := res.Error
	if reason == "" {
		reason = res.Response
	}
	err := fmt.Errorf("agy: the run failed (%s): %s", res.Status, util.LogHead(reason))
	if loggedOut(reason) {
		return fmt.Errorf("%w: %w", ErrLoggedOut, err)
	}
	return err
}

// AskAgy answers a question through the Antigravity command line on the user's own plan, running June's tools through the same gate and trace as every other ask. Output: the turn trace with the answer, tool hops, evidence and model "agy/<model>" (or "agy" when no model was named), or the trace so far and an error.
func (a *Agent) AskAgy(ctx context.Context, question string) (TurnTrace, error) {
	return a.AskAgyWith(ctx, nil, question)
}

// AskAgyWith is AskAgy with the conversation so far sent ahead of the question, so a follow-up reads as one. Input: the prior turns (see HistoryFromTurns), nil for a question that stands alone, and the question. Output: the same TurnTrace AskAgy returns.
func (a *Agent) AskAgyWith(ctx context.Context, history History, question string) (TurnTrace, error) {
	return a.askAgy(ctx, newAgyProcess, agyModel(), history, question)
}

// askAgy is AskAgyWith against the given session constructor and model, so a test can drive a whole ask without the command line. newProc is called only when a fresh process has to be started — reusing a live one that already holds this conversation never calls it again, and a conversation no live process holds always gets a fresh one.
func (a *Agent) askAgy(ctx context.Context, newProc func() agySessionRunner, model string, history History, question string) (TurnTrace, error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, agyAskTimeout)
	defer cancel()
	// The look allowance, the picture draw maps coordinates against, and what the pictures cost all belong to one ask, carried on ctx from here on so a concurrent ask never shares this one's screenshot.
	ctx = withAskLookState(ctx)
	instruction := a.LeanPrompt(start)
	var handshake []string

	recallCtx, cancelRecall := context.WithTimeout(ctx, recallTimeout)
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
	slog.Debug("ask agy: done", "model", model, "turns", res.NumTurns, "tools", len(tr.ToolHops), "input_tokens", res.Usage.InputTokens, "output_tokens", res.Usage.OutputTokens, "cached_input_tokens", res.Usage.CacheReadTokens, "denied", res.deniedTools(), "duration", tr.Duration)
	if tr.Answer == "" {
		note := ""
		if denied := res.deniedTools(); denied != "" {
			note = " (it reached for tools it may not use here: " + denied + ")"
		}
		return tr, fmt.Errorf("%w: agy: the run returned no text%s", ErrNoAnswer, note)
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

const (
	// agyUsagePoll is the shortest gap between two reads of the plan's allowance, so a picker polled every few seconds and a run of asks do not each start the command line.
	agyUsagePoll = 10 * time.Minute
	// agyUsageTimeout bounds one read: agy's own startup is a few seconds, and a read that hangs must not keep a process around.
	agyUsageTimeout = 30 * time.Second
)

// agyUsagePolled is when the allowance was last read and whether a read is under way, so RefreshAgyUsage holds itself to agyUsagePoll and never runs two at once.
var agyUsagePolled struct {
	sync.Mutex
	at      time.Time
	running bool
}

// agyUsageOff is set once a read of the allowance turned out to have run a model turn (see readAgyUsage), which stops every later read until the daemon restarts.
var agyUsageOff atomic.Bool

// agyUsageRanATurn reports whether what `agy -p /usage` printed is a model turn's result rather than the command's: the command answers with num_turns 0 and no tokens (measured on 2026-10-03). Input: the output. Output: true when it says a turn ran or tokens were spent; output that is not JSON says neither.
func agyUsageRanATurn(payload []byte) bool {
	var res struct {
		NumTurns int `json:"num_turns"`
		Usage    struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(payload, &res) != nil {
		return false
	}
	return res.NumTurns > 0 || res.Usage.InputTokens > 0 || res.Usage.OutputTokens > 0 || res.Usage.TotalTokens > 0
}

// RefreshAgyUsage reads the Antigravity plan's allowance into the recorder set by SetUsageRecorder, in the background and at most once every agyUsagePoll however often it is called. It is called after every agy turn and whenever the picker reads the Antigravity row. Input: none. Output: none — a read that fails is logged at debug and leaves the last good reading standing.
func RefreshAgyUsage() {
	usageRecorder.Lock()
	to := usageRecorder.to
	usageRecorder.Unlock()
	if to == nil || !agyReady() || agyUsageOff.Load() {
		return
	}
	agyUsagePolled.Lock()
	if agyUsagePolled.running || time.Since(agyUsagePolled.at) < agyUsagePoll {
		agyUsagePolled.Unlock()
		return
	}
	agyUsagePolled.at = time.Now()
	agyUsagePolled.running = true
	agyUsagePolled.Unlock()
	go func() {
		defer func() {
			agyUsagePolled.Lock()
			agyUsagePolled.running = false
			agyUsagePolled.Unlock()
		}()
		limits, err := readAgyUsage(context.Background())
		if err != nil {
			slog.Debug("agy: could not read the plan's allowance", "error", err)
			return
		}
		recordUsage(agyBrainID, limits)
	}()
}

// readAgyUsage asks the command line for the plan's allowance with its own /usage command. agy reports the allowance nowhere else June can read: the stream-json events carry token counts only, the statusline command June used to install is a terminal-interface feature that a print or stream-json run never invokes (no capture landed in any of twenty session homes on 2026-10-03), and the endpoint behind it needs the OAuth token out of the user's keyring. `agy -p /usage --output-format json` answers locally, without starting an agent turn or spending quota (agy's changelog; measured on 2026-10-03 at num_turns 0 and zero tokens).
// It runs in a duty's throwaway HOME as AgyDutyAgent rather than under the user's own HOME: should a later agy, or an argument mangled on the way, take "/usage" for a prompt, every poll would otherwise be a whole agent turn of about 16,000 tokens with the user's own grants and MCP servers (a probe run through Git Bash, which rewrites a lone /usage into a Windows path, ran a whole turn that reached for run_command, 2026-10-03). A reading that shows a turn ran stops the polling until the daemon restarts.
// Input: a context. Output: the windows agyQuotaLimits reads out of the answer, or an error naming what failed.
func readAgyUsage(ctx context.Context) ([]UsageLimit, error) {
	ctx, cancel := context.WithTimeout(ctx, agyUsageTimeout)
	defer cancel()
	env, dir, remove, err := AgyDutyHome()
	if err != nil {
		return nil, err
	}
	defer remove()
	cmd := exec.CommandContext(ctx, agyBinary, "--print", "/usage", "--output-format", "json", "--agent", AgyDutyAgent)
	util.OwnProcessGroup(cmd)
	cmd.Cancel = func() error { return util.KillProcessGroup(cmd) }
	cmd.Env = env
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.WaitDelay = 2 * time.Second
	release, err := util.StartProcessGroup(cmd)
	if err == nil {
		defer release()
		err = cmd.Wait()
	}
	if err != nil {
		return nil, fmt.Errorf("agy /usage: %w", err)
	}
	if agyUsageRanATurn(out.Bytes()) {
		agyUsageOff.Store(true)
		slog.Warn("agy: reading the plan's allowance ran a model turn instead of the /usage command, so it is not read again until June restarts")
		return nil, errors.New("agy /usage: the read ran a model turn")
	}
	limits := agyQuotaLimits(out.Bytes())
	if len(limits) == 0 {
		return nil, errors.New("agy /usage: the answer carried no allowance windows")
	}
	return limits, nil
}

// agyQuotaLimits reads the plan allowance out of what `agy -p /usage --output-format json` prints. Input: that output. Output: one window per bucket, sorted by bucket id, or nothing at all when the output is not JSON or carries no buckets — an empty result must leave the last good reading alone rather than record empty bars.
// agy reports how much of a window is left and June draws how much is spent, so the fractions are inverted here. The window names are agy's own bucket ids ("gemini-5h", "3p-weekly"): the plan meters the Gemini models and the third-party ones it also carries separately, and collapsing them would hide a family that is spent behind one that is not. A bucket with no remaining_fraction is one with nothing left: agy writes the field with omitempty, which drops exactly the zero.
func agyQuotaLimits(payload []byte) []UsageLimit {
	var doc struct {
		Command struct {
			Data struct {
				Groups []struct {
					Buckets []struct {
						ID                string   `json:"id"`
						RemainingFraction *float64 `json:"remaining_fraction"`
						ResetTime         string   `json:"reset_time"`
					} `json:"buckets"`
				} `json:"groups"`
			} `json:"data"`
		} `json:"command"`
	}
	if err := json.Unmarshal(payload, &doc); err != nil {
		return nil
	}
	var limits []UsageLimit
	for _, group := range doc.Command.Data.Groups {
		for _, b := range group.Buckets {
			if b.ID == "" {
				continue
			}
			remaining := 0.0
			if b.RemainingFraction != nil {
				remaining = *b.RemainingFraction
			}
			resets, _ := time.Parse(time.RFC3339, b.ResetTime)
			limits = append(limits, UsageLimit{
				Window:       b.ID,
				UsedFraction: 1 - remaining,
				ResetsAt:     resets,
				Source:       "agy /usage " + b.ID,
			})
		}
	}
	slices.SortFunc(limits, func(x, y UsageLimit) int { return strings.Compare(x.Window, y.Window) })
	return limits
}
