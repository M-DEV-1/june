// claude.go answers /ask with Anthropic models on the user's own Claude subscription by running the Claude Code CLI, and offers it June's own tools — the screen tools and the memory tools — as an MCP server the daemon holds open for the length of one ask.
// The CLI is the only way to reach the subscription: a third-party login billed through the API key is extra usage on top of what the user already pays for, which is why --bare is never passed here (it makes the CLI read ANTHROPIC_API_KEY instead of the login).
package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"june/internal/util"

	"google.golang.org/genai"
)

const (
	// claudeBinary is the Claude Code command line, found on PATH.
	claudeBinary = "claude"
	// claudeDefaultModel is the model an ask uses when nothing names another: sonnet, because computer use is a great many cheap rounds rather than a few hard ones.
	claudeDefaultModel = "sonnet"
	// claudeAskTimeout bounds one whole ask, every tool round included, and matches what the Codex path allows itself.
	claudeAskTimeout = 12 * time.Minute
	// claudeMCPServerName is what the CLI calls June's tool server, and so is the prefix on every tool name the model sees: mcp__june__observe_screen.
	claudeMCPServerName = "june"
	// claudeProtocolVersion is the MCP version the server falls back to when the client names none of its own.
	claudeProtocolVersion = "2025-11-25"
)

// ProviderClaude is the Claude Code command line, which serves Anthropic models on the user's own subscription rather than on an API key.
const ProviderClaude = "claude"

// claudeWebTool is the one built-in tool June keeps: the CLI's own web search, which runs on the user's subscription and costs nothing extra. Named once because it goes in both --tools and --allowed-tools, and the two disagreeing means no search and no error saying so.
const claudeWebTool = "WebSearch"

// claudeModel is the model an ask asks for. Input: none. Output: the model last picked for Claude in the window, else JUNE_CLAUDE_MODEL, else claudeDefaultModel.
func claudeModel() string {
	return pickedModel("claude", "JUNE_CLAUDE_MODEL", claudeDefaultModel)
}

// claudeLoggedIn reports whether this machine has a Claude Code login to run under, which is what makes Claude worth handing a question to.
func claudeLoggedIn() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	_, err = os.Stat(home + "/.claude/.credentials.json")
	return err == nil
}

// claudeToolServer offers June's own tools to one `claude -p` run over MCP's HTTP transport, running each call through the same gate as every other ask and keeping the calls it ran so the turn's trace can carry them.
// It listens on the loopback interface at an unguessable path, which is what stops anything else on the machine from driving the user's screen through it. The listener lives only as long as the ask.
// ponytail: one server per ask on a fresh port; if asks ever run often enough for that to matter, one long-lived server with a per-ask path would do.
type claudeToolServer struct {
	agent *Agent
	// askCtx is the ask's own context, not the HTTP request's, so a tool result still reaches the window's progress stream under the right ask id.
	askCtx   context.Context
	decls    []*genai.FunctionDeclaration
	path     string
	listener net.Listener
	server   *http.Server
	// mu guards annotations, steps and hops together, because the CLI may have more than one call in flight.
	mu sync.Mutex
	// annotations is how many marks this run has drawn on the screen. They are counted apart from the step cap because drawing over the screen changes nothing the model has to read back, and counted at all because this path has no round bound of its own — the two loops in ask.go and the one in codex.go stop at maxAskRounds whatever their step count says, and without this a run that did nothing but draw would never end.
	annotations int
	// steps is how many of this run's tool calls have counted against maxAskIterations so far (annotations and a repeated observe_screen of the same screen do not, the same as the other ask paths — see sameScreenAgain, onlyAnnotated).
	steps int
	// hops are the tool calls this run made, in call order.
	hops []ToolHop
}

// startClaudeToolServer starts the tool server for one ask on a loopback port. Input: the ask's context, used to run the tools it is asked to run. Output: the running server, which the caller must Close, or an error when the port could not be taken.
func (a *Agent) startClaudeToolServer(ctx context.Context) (*claudeToolServer, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return nil, fmt.Errorf("claude: naming the tool server: %w", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("claude: opening the tool server: %w", err)
	}
	s := &claudeToolServer{
		agent:    a,
		askCtx:   ctx,
		decls:    a.askToolDeclarations(),
		path:     "/" + hex.EncodeToString(buf[:]),
		listener: listener,
	}
	mux := http.NewServeMux()
	mux.HandleFunc(s.path, s.handle)
	s.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go s.server.Serve(listener)
	return s, nil
}

// URL is where the CLI reaches this server.
func (s *claudeToolServer) URL() string {
	return "http://" + s.listener.Addr().String() + s.path
}

// Close stops the server and frees its port.
func (s *claudeToolServer) Close() { s.server.Close() }

// Hops is the tool calls this run made so far, in call order.
func (s *claudeToolServer) Hops() []ToolHop {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hops
}

// record adds one finished tool call.
func (s *claudeToolServer) record(hop ToolHop) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hops = append(s.hops, hop)
}

// reserveAnnotation claims one of this run's allowance for marks on the screen, which is counted apart from the step cap because an annotation changes nothing the model must then re-read (see annotationTools). The allowance is maxAskRounds, the same hard bound the round-based loops stop at, so a run that does nothing but draw still ends. Output: true when one was claimed, false when the allowance is spent.
func (s *claudeToolServer) reserveAnnotation() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.annotations >= maxAskRounds {
		return false
	}
	s.annotations++
	return true
}

// reserveStep claims one of this run's maxAskIterations tool-call steps, the same hard cap every other ask path stops at. Annotation tools never call this (see call()); everything else does, one claim per call, with no dedup for a repeated observe_screen of the same screen — the round-based paths' sameScreenAgain has nothing to batch here, since this server sees one call at a time. Output: true when a step was claimed, false once the cap is spent, which is call()'s cue to refuse every tool call from here on.
func (s *claudeToolServer) reserveStep() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.steps >= maxAskIterations {
		return false
	}
	s.steps++
	return true
}

// Capped reports whether this run spent its full maxAskIterations tool-call steps, so askClaude can end the turn on capError the way every round-based ask path does when its loop runs out, rather than trusting an answer the CLI gave after being refused every tool call past the cap.
func (s *claudeToolServer) Capped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.steps >= maxAskIterations
}

// handle answers one JSON-RPC request from the CLI. Input: an HTTP POST carrying an MCP request. Output: the JSON-RPC reply, 202 for a notification (which carries no id and expects no answer), and 405 for anything that is not a POST.
func (s *claudeToolServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			ProtocolVersion string          `json:"protocolVersion"`
			Name            string          `json:"name"`
			Arguments       map[string]any  `json:"arguments"`
			Meta            json.RawMessage `json:"_meta"`
		} `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any
	switch req.Method {
	case "initialize":
		version := req.Params.ProtocolVersion
		if version == "" {
			version = claudeProtocolVersion
		}
		result = map[string]any{"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": claudeMCPServerName, "version": "1"}}
	case "tools/list":
		result = map[string]any{"tools": claudeTools(s.decls)}
	case "tools/call":
		content := []any{map[string]any{"type": "text", "text": s.call(req.Params.Name, req.Params.Arguments)}}
		// A tool that took a picture of the screen hands it back beside its text, as MCP's own image content item, which is how the command line gets to see what a look saw.
		if shot, ok := takeLook(s.askCtx); ok {
			content = append(content, map[string]any{"type": "image", "data": base64.StdEncoding.EncodeToString(shot.Data), "mimeType": shot.Mime})
		}
		result = map[string]any{"content": content}
	default:
		// Anything else — a discovery probe, a resources or prompts listing — is a method this server does not have, which is a thing MCP clients are built to carry on past.
		writeClaudeRPC(w, req.ID, nil, &claudeRPCError{Code: -32601, Message: "no such method: " + req.Method})
		return
	}
	writeClaudeRPC(w, req.ID, result, nil)
}

// call runs one tool for the model and returns what to tell it. Input: the tool's name and arguments as the CLI sent them. Output: the tool's result, or a refusal when the tool is outside the ask's gate, has drawn on the screen as much as its allowance permits, or has spent the run's maxAskIterations tool-call steps — askClaude ends that run on capError once Capped() says so, whatever the CLI itself goes on to answer.
func (s *claudeToolServer) call(name string, args map[string]any) string {
	if args == nil {
		args = map[string]any{}
	}
	if annotationTools[name] {
		if !s.reserveAnnotation() {
			return "error: this turn has marked the screen as much as it can; answer now with what you already have."
		}
	} else if !s.reserveStep() {
		return "error: this turn has run out of steps; answer now with what you already have."
	}
	ObserveTool(s.askCtx, name, toolActivitySummary(name, args), false)
	result := s.agent.evalExecute(WithOffered(s.askCtx, declNames(s.decls)), name, args)
	ObserveTool(s.askCtx, name, resultSummary(name, result), strings.HasPrefix(result, "error"))
	slog.Info("ask: tool", "tool", name, "args", toolActivitySummary(name, args), "result", resultSummary(name, result), "detail", toolLogDetail(name, result))
	s.record(ToolHop{Name: name, Args: args, Result: result})
	return result
}

// claudeRPCError is the error object a JSON-RPC reply carries instead of a result.
type claudeRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// writeClaudeRPC writes one JSON-RPC reply. Input: the response writer, the request's id echoed back, and exactly one of a result or an error.
func writeClaudeRPC(w http.ResponseWriter, id json.RawMessage, result any, rpcErr *claudeRPCError) {
	reply := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		reply["error"] = rpcErr
	} else {
		reply["result"] = result
	}
	util.WriteJSON(w, reply)
}

// claudeTools converts the agent's tool declarations into the MCP tool objects tools/list answers with, one per declaration in the same order. The argument schema is the same JSON Schema the Codex path sends.
func claudeTools(decls []*genai.FunctionDeclaration) []any {
	out := make([]any, 0, len(decls))
	for _, d := range decls {
		out = append(out, map[string]any{"name": d.Name, "description": d.Description, "inputSchema": jsonSchema(d.Parameters)})
	}
	return out
}

// writeClaudeAskFiles writes the MCP config and the system prompt to two 0600 files inside a fresh temp directory of their own, so neither ever sits in argv: any local process can read another process's argv for the life of a run, on Linux via /proc/<pid>/cmdline, and the config carries the tool server's own unguessable URL while the system prompt carries the user's personal context. Input: where the tool server is listening and the system prompt to send. Output: the directory — the caller removes it once the ask ends — and the two file paths, or an error naming what could not be written.
func writeClaudeAskFiles(mcpURL, instruction string) (dir, mcpConfigPath, systemPromptPath string, err error) {
	dir, err = os.MkdirTemp(os.TempDir(), "june-claude-ask-")
	if err != nil {
		return "", "", "", fmt.Errorf("claude: making the ask's own temp dir: %w", err)
	}
	config := fmt.Sprintf(`{"mcpServers":{%q:{"type":"http","url":%q}}}`, claudeMCPServerName, mcpURL)
	mcpConfigPath = filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(mcpConfigPath, []byte(config), 0o600); err != nil {
		os.RemoveAll(dir)
		return "", "", "", fmt.Errorf("claude: writing the mcp config: %w", err)
	}
	systemPromptPath = filepath.Join(dir, "system-prompt.txt")
	if err := os.WriteFile(systemPromptPath, []byte(instruction), 0o600); err != nil {
		os.RemoveAll(dir)
		return "", "", "", fmt.Errorf("claude: writing the system prompt: %w", err)
	}
	return dir, mcpConfigPath, systemPromptPath, nil
}

// claudeArgs is the argument list for one `claude -p` run.
// Input: the model to ask for, the files writeClaudeAskFiles wrote (the MCP config and the system prompt — the CLI reads both from disk, keeping the tool server's URL and the user's personal context off argv), and the tools to allow. Output: the arguments.
// The user's own settings, hooks, skills, plugins and MCP servers are all shut out, because the prompt carries text nobody vetted — a meeting transcript, whatever was on the user's screens — and because a hook or a skill of the user's own would change what June's answers are made of without June knowing. --restricted drops the built-in command-running tools, WebFetch, and the settings files; --tools names the only built-in kept, leaving the model with June's tools and that one.
// WebSearch is the exception, and it is the whole reason an ask that falls back to Claude can look something up: Gemini's grounding is the only web access June otherwise has, and on 2026-09-07 its free-tier allowance was spent while the models themselves still answered, so every fallback answered from memory and said it had no web access. This is a deliberate widening — a prompt carrying text nobody vetted can shape a search query, and a query goes to a search engine — accepted because the alternative is an assistant that cannot look anything up whenever one provider's allowance runs out.
func claudeArgs(model, mcpConfigPath, systemPromptPath string, toolNames []string) []string {
	allowed := make([]string, 0, len(toolNames)+1)
	for _, name := range toolNames {
		allowed = append(allowed, "mcp__"+claudeMCPServerName+"__"+name)
	}
	allowed = append(allowed, claudeWebTool)
	return []string{
		"-p",
		"--output-format", "json",
		"--model", model,
		"--mcp-config", mcpConfigPath,
		"--strict-mcp-config",
		"--restricted",
		"--tools", claudeWebTool,
		"--allowed-tools", strings.Join(allowed, ","),
		"--permission-prompts", "none",
		"--disable-slash-commands",
		"--no-session-persistence",
		"--system-prompt-file", systemPromptPath,
	}
}

// claudeResult is the object `claude -p --output-format json` prints when the run is over.
type claudeResult struct {
	Result   string `json:"result"`
	IsError  bool   `json:"is_error"`
	Subtype  string `json:"subtype"`
	NumTurns int    `json:"num_turns"`
	Usage    struct {
		InputTokens      int `json:"input_tokens"`
		OutputTokens     int `json:"output_tokens"`
		CacheReadTokens  int `json:"cache_read_input_tokens"`
		CacheWriteTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

// claudeRunner runs one CLI process to completion. Input: the arguments and the prompt to feed it on stdin. Output: its stdout, or an error naming what went wrong. A test replaces it with a stub so a whole ask, tool calls included, runs without the CLI.
type claudeRunner func(ctx context.Context, args []string, stdin string) ([]byte, error)

// runClaudeCLI is the runner that actually starts the command line.
// The prompt goes in on stdin because a single argv entry is capped at 128 KB on Linux and a thread with recalled memory in it can be bigger than that. The working directory is an empty temporary one, so the CLI finds no project files or instruction files of the user's to read.
func runClaudeCLI(binary string) claudeRunner {
	return func(ctx context.Context, args []string, stdin string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, binary, args...)
		util.HideConsole(cmd)
		cmd.Stdin = strings.NewReader(stdin)
		cmd.Dir = os.TempDir()
		var out, stderr bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &stderr
		// Killing the child does not kill its own children, and stdout stays open as long as any of them holds it, so without this a hung run would block here for as long as its grandchildren live.
		cmd.WaitDelay = 2 * time.Second
		if err := cmd.Run(); err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, fmt.Errorf("claude: the run timed out after %s", claudeAskTimeout)
			}
			// A run that failed still prints its result object on stdout, and that object says more about why than the exit status does, so it is preferred when it is there.
			if len(out.Bytes()) > 0 {
				return out.Bytes(), nil
			}
			return nil, fmt.Errorf("claude: %w: %s", err, util.LogHead(stderr.String()))
		}
		return out.Bytes(), nil
	}
}

// claudeSourceLinkPattern matches one "[Title](url)" markdown link inside a Sources block.
var claudeSourceLinkPattern = regexp.MustCompile(`\[([^\]]*)\]\(([^)]+)\)`)

// stripSourcesBlock splits off Claude's own trailing "Sources:" block — the plain-text list of markdown links its built-in WebSearch tool appends after an answer that used it — since that tool runs outside June's own MCP server and so leaves nothing in tr.ToolHops for evidenceFromToolHops to read a citation from. Input: the raw answer text. Output: the answer with the block (and the blank line before it) removed, and one Evidence entry per link found in the block, kind "web", in the order they appeared. Both are returned unchanged when the text carries no such block: "Sources:" must open a line of its own (not just occur inside the prose) and that line's block must contain at least one markdown link, or nothing is stripped.
func stripSourcesBlock(answer string) (string, []Evidence) {
	idx := strings.LastIndex(answer, "Sources:")
	if idx == -1 || (idx > 0 && answer[idx-1] != '\n') {
		return answer, nil
	}
	block := answer[idx:]
	links := claudeSourceLinkPattern.FindAllStringSubmatch(block, -1)
	if len(links) == 0 {
		return answer, nil
	}
	rest := strings.TrimRight(answer[:idx], "\n \t")
	evidence := make([]Evidence, 0, len(links))
	for _, m := range links {
		evidence = append(evidence, Evidence{Kind: "web", Title: strings.TrimSpace(m[1]), Excerpt: strings.TrimSpace(m[2])})
	}
	return rest, evidence
}

// claudeScreenTools is the tool set a Claude ask offers when isScreenTask says the question is a screen task: screenRoundTools (see ask.go) plus the tools that read memory rather than the screen — query_memory, query_store, recall and personal_context. A round deciding which numbered button to press does not need the whole memory surface, but it still needs a way to pull one fact into view, the name of the user's earbuds, a contact, so those four stay rather than going with the rest of the memory teaching. The tools that write to memory beyond save_note (already in screenRoundTools) go with it: add_task, revise, action_items and branch all need the full handshake to know when they apply.
var claudeScreenTools = unionToolSets(screenRoundTools, map[string]bool{
	"query_memory": true, "query_store": true, "recall": true, "personal_context": true,
})

// unionToolSets merges any number of tool-name sets into one. Input: the sets to merge. Output: a new set containing every name any of them held.
func unionToolSets(sets ...map[string]bool) map[string]bool {
	out := make(map[string]bool)
	for _, s := range sets {
		for name := range s {
			out[name] = true
		}
	}
	return out
}

// AskClaude answers a question through the Claude Code command line on the user's own subscription, running June's tools through the same gate and trace as every other ask. Output: the turn trace with the answer, tool hops, evidence and model "claude/<model>", or the trace so far and an error.
func (a *Agent) AskClaude(ctx context.Context, question string) (TurnTrace, error) {
	return a.AskClaudeWith(ctx, nil, question)
}

// AskClaudeWith is AskClaude with the conversation so far sent ahead of the question, so a follow-up reads as one. Input: the prior turns (see HistoryFromTurns), nil for a question that stands alone, and the question. Output: the same TurnTrace AskClaude returns.
func (a *Agent) AskClaudeWith(ctx context.Context, history History, question string) (TurnTrace, error) {
	return a.askClaude(ctx, runClaudeCLI(claudeBinary), claudeModel(), history, question)
}

// claudeThread renders the prior turns as plain text to put ahead of the question, because the command line takes one prompt rather than a list of messages. Input: the history, oldest first. Output: the thread as labelled lines, or "" when there is none.
func claudeThread(history History) string {
	var b strings.Builder
	for _, c := range history {
		if c == nil {
			continue
		}
		var text strings.Builder
		for _, p := range c.Parts {
			if p != nil {
				text.WriteString(p.Text)
			}
		}
		if text.Len() == 0 {
			continue
		}
		who := "June"
		if c.Role == genai.RoleUser {
			who = "The user"
		}
		fmt.Fprintf(&b, "%s: %s\n", who, text.String())
	}
	if b.Len() == 0 {
		return ""
	}
	return "[thread] What was said earlier in this conversation:\n" + b.String()
}

// askClaude is AskClaudeWith against the given runner and model, so a test can drive a whole ask without the command line.
func (a *Agent) askClaude(ctx context.Context, run claudeRunner, model string, history History, question string) (TurnTrace, error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, claudeAskTimeout)
	defer cancel()
	// The look allowance, the picture draw maps coordinates against, and what the pictures cost all belong to one ask, carried on ctx from here on so a concurrent ask never shares this one's screenshot.
	ctx = withAskLookState(ctx)
	instruction := a.LeanPrompt(start)
	var handshake []string
	// A screen task (see isScreenTask in ask.go) gets the short screen prompt instead of the full handshake, and later, once the tool server exists, the short screen tool set instead of every tool the ask gate allows.
	screenTask := isScreenTask(question, nil)
	if screenTask {
		instruction = screenTaskInstruction()
	}

	recallCtx, cancelRecall := context.WithTimeout(ctx, textSendLoopRetrieveTimeout)
	injected, err := a.brain.RetrieveRelevant(recallCtx, question, 2)
	cancelRecall()
	if err != nil {
		slog.Warn("AskClaude retrieve relevant failed, continuing without inject", "error", err)
		injected = nil
	}

	tr := TurnTrace{
		Channel:   ChannelText,
		Model:     "claude/" + model,
		Question:  question,
		Handshake: handshake,
		Injected:  injected,
		Usage:     TokenUsage{Provider: ProviderClaude},
	}

	server, err := a.startClaudeToolServer(ctx)
	if err != nil {
		tr.Duration = time.Since(start)
		return tr, err
	}
	defer server.Close()
	if screenTask {
		server.decls = trimToDeclarations(server.decls, claudeScreenTools)
	}

	// The question is the last thing the model reads: the thread comes first, then this turn's time and recalled memory, then the reference to a close past run, then what was actually asked.
	var prompt strings.Builder
	if thread := claudeThread(history); thread != "" {
		prompt.WriteString(thread + "\n")
	}
	prompt.WriteString(turnContext(start, injected) + "\n\n")
	reference, shownLessons := a.actReference(ctx, question, start)
	tr.LessonsShown = shownLessons
	if reference != "" {
		prompt.WriteString(reference + "\n\n")
	}
	prompt.WriteString(question)

	dir, mcpConfigPath, systemPromptPath, err := writeClaudeAskFiles(server.URL(), instruction)
	if err != nil {
		tr.Duration = time.Since(start)
		return tr, err
	}
	defer os.RemoveAll(dir)

	names := make([]string, 0, len(server.decls))
	for _, d := range server.decls {
		names = append(names, d.Name)
	}
	out, err := run(ctx, claudeArgs(model, mcpConfigPath, systemPromptPath, names), prompt.String())
	tr.ToolHops = server.Hops()
	tr.Evidence = evidenceFromToolHops(tr.ToolHops)
	tr.ImageTokens = lookTokensSpent(ctx)
	tr.Duration = time.Since(start)
	if server.Capped() {
		return tr, capError(tr.ToolHops)
	}
	if err != nil {
		return tr, err
	}
	var res claudeResult
	if err := json.Unmarshal(out, &res); err != nil {
		return tr, fmt.Errorf("claude: could not parse what the command line printed: %w (%s)", err, util.LogHead(string(out)))
	}
	tr.Usage.Rounds = res.NumTurns
	// The CLI reports the input in three parts: what it read afresh, what it wrote into its prompt cache, and what it answered out of that cache. All three are input the model read, so the whole input is their sum and the cached part is one of them, which is the same shape the Codex path records. Measured against the real command line on 2026-09-05: a two-round ask reported input_tokens 4, cache_creation 7,283 and cache_read 7,143, so counting input_tokens alone would put a 14,430-token ask on record as having cost four.
	input := res.Usage.InputTokens + res.Usage.CacheWriteTokens + res.Usage.CacheReadTokens
	tr.Usage.add(input, res.Usage.OutputTokens, input+res.Usage.OutputTokens)
	tr.Usage.CachedInputTokens = res.Usage.CacheReadTokens
	if res.IsError {
		return tr, fmt.Errorf("claude: the run failed (%s): %s", res.Subtype, util.LogHead(res.Result))
	}
	answer, sources := stripSourcesBlock(strings.TrimSpace(res.Result))
	tr.Answer = answer
	tr.Evidence = append(tr.Evidence, sources...)
	slog.Debug("ask claude: done", "model", model, "turns", res.NumTurns, "tools", len(tr.ToolHops), "input_tokens", res.Usage.InputTokens, "output_tokens", res.Usage.OutputTokens, "cached_input_tokens", res.Usage.CacheReadTokens, "duration", tr.Duration)
	if tr.Answer == "" {
		return tr, errors.New("claude: the run returned no text")
	}
	return tr, nil
}

// ClaudeBrain is the asker the daemon registers under the "claude" brain name; it answers through the agent's AskClaude.
type ClaudeBrain struct {
	Agent *Agent
}

// AskText answers the question through AskClaude, so the ipc server can route a "claude" ask like any other brain.
func (b ClaudeBrain) AskText(ctx context.Context, question string) (TurnTrace, error) {
	return b.Agent.AskClaude(ctx, question)
}

// AskTextWith answers the question with the conversation so far, so the ipc server can hand a "claude" ask its thread exactly as it hands one to the default brain. Input: the prior turns and the question. Output: the turn trace from AskClaudeWith.
func (b ClaudeBrain) AskTextWith(ctx context.Context, history History, question string) (TurnTrace, error) {
	return b.Agent.AskClaudeWith(ctx, history, question)
}

// CodexThenClaude asks Codex first and hands the question to Claude when Codex's allowance is spent, so the unattended jobs and the /ask fallback chain do not stop at the smaller of the two subscriptions. It is the asker shape the ipc server and internal/brain both take.
type CodexThenClaude struct {
	Agent *Agent
}

// AskText answers through Codex, or through Claude when Codex refused because its allowance is spent. Input: the question. Output: whichever turn trace answered, or Codex's own error when Claude is not a way out of it.
func (b CodexThenClaude) AskText(ctx context.Context, question string) (TurnTrace, error) {
	tr, err := b.Agent.AskCodex(ctx, question)
	// The same rule the router applies: hand on only when the failure is a spent allowance another provider does not share, only while no action has run so a click is never taken twice, and only when this machine has a Claude login.
	if ProviderSpent(err) && actionHops(tr.ToolHops) == 0 && claudeLoggedIn() {
		slog.Warn("ask: the Codex allowance is spent, asking Claude", "error", err)
		return b.Agent.AskClaude(ctx, question)
	}
	return tr, err
}

const (
	// claudeUsageURL is the OAuth usage endpoint Claude Code's own /usage screen reads: it answers with the subscription's five-hour and seven-day windows for the account the access token belongs to. It is undocumented; the request and response shapes are recorded at https://github.com/Maciek-roboblog/Claude-Code-Usage-Monitor/issues/202.
	claudeUsageURL = "https://api.anthropic.com/api/oauth/usage"
	// claudeOAuthBeta is the beta header the OAuth routes require.
	claudeOAuthBeta = "oauth-2025-04-20"
	// claudeUsageAgent is the user agent the endpoint expects. Anything that does not look like Claude Code lands in a much smaller rate-limit bucket and gets 429s, so the version is sent even though it is not the one installed here.
	// ponytail: a pinned version string; read it from `claude --version` if the endpoint ever starts checking it.
	claudeUsageAgent = "claude-code/2.1.261"
	// claudeUsagePoll is the shortest gap between two reads of that endpoint, so a window polling /brains every few seconds does not poll Anthropic with it.
	claudeUsagePoll = 10 * time.Minute
	// claudeUsageTimeout bounds one read, so a slow endpoint cannot hold up the brain picker.
	claudeUsageTimeout = 5 * time.Second
)

// ClaudeCredentialsPath is where the Claude CLI writes its login under the home directory home. Every place that checks whether the user is signed in to Claude, or reads a field out of that login, goes through this so the path is typed once.
func ClaudeCredentialsPath(home string) string {
	return filepath.Join(home, ".claude", ".credentials.json")
}

// claudeCredentials is the part of ~/.claude/.credentials.json this file reads: the OAuth block Claude Code writes the subscription login into. Only the access token is taken, and it is never logged, never written anywhere, and never leaves the Authorization header of the one request below — the same rule loadCodexAuth follows for Codex's own auth file.
type claudeCredentials struct {
	OAuth struct {
		AccessToken string `json:"accessToken"`
	} `json:"claudeAiOauth"`
}

// loadClaudeToken reads the subscription's OAuth access token out of the credentials file at path. Output: the token, or an error naming the file when it is missing, is not the file Claude Code writes, or holds no token. The error never carries the file's contents.
func loadClaudeToken(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("claude login: %w", err)
	}
	var creds claudeCredentials
	if err := json.Unmarshal(raw, &creds); err != nil {
		return "", fmt.Errorf("claude login: %s is not the credentials file Claude Code writes", path)
	}
	if creds.OAuth.AccessToken == "" {
		return "", fmt.Errorf("claude login: %s has no subscription access token; run `claude auth` first", path)
	}
	return creds.OAuth.AccessToken, nil
}

// claudeUsageWindow is one allowance window the usage endpoint reports: utilization is a percentage from 0 to 100, and resets_at is when the window starts again.
type claudeUsageWindow struct {
	Utilization float64   `json:"utilization"`
	ResetsAt    time.Time `json:"resets_at"`
}

// claudeUsageResponse is the endpoint's body. A window the account has no limit on comes back as null, which is why every field is a pointer.
type claudeUsageResponse struct {
	FiveHour       *claudeUsageWindow `json:"five_hour"`
	SevenDay       *claudeUsageWindow `json:"seven_day"`
	SevenDayOpus   *claudeUsageWindow `json:"seven_day_opus"`
	SevenDaySonnet *claudeUsageWindow `json:"seven_day_sonnet"`
}

// claudeUsage reads the subscription's allowance windows from the OAuth usage endpoint. Input: a context, the HTTP client to use, the endpoint (the test points this at a stub), and the credentials file to take the access token from. Output: one UsageLimit per window the account has, in the order the picker draws them, or an error. The error never carries the token or the response body.
func claudeUsage(ctx context.Context, client *http.Client, url, credsPath string) ([]UsageLimit, error) {
	token, err := loadClaudeToken(credsPath)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", claudeOAuthBeta)
	req.Header.Set("User-Agent", claudeUsageAgent)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("claude usage: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%w: claude usage: HTTP %d", ErrLoggedOut, resp.StatusCode)
	}
	if resp.StatusCode/100 != 2 {
		// The body is dropped rather than quoted: a refusal from this endpoint can echo the Authorization header back.
		return nil, fmt.Errorf("claude usage: HTTP %d", resp.StatusCode)
	}
	var body claudeUsageResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&body); err != nil {
		return nil, fmt.Errorf("claude usage: the endpoint did not answer with the usage windows: %w", err)
	}
	var limits []UsageLimit
	for _, w := range []struct {
		name   string
		field  string
		window *claudeUsageWindow
	}{
		{"5h", "five_hour", body.FiveHour},
		{"weekly", "seven_day", body.SevenDay},
		{"weekly_opus", "seven_day_opus", body.SevenDayOpus},
		{"weekly_sonnet", "seven_day_sonnet", body.SevenDaySonnet},
	} {
		if w.window == nil {
			continue
		}
		limits = append(limits, UsageLimit{
			Window:       w.name,
			UsedFraction: w.window.Utilization / 100,
			ResetsAt:     w.window.ResetsAt,
			Source:       "api/oauth/usage " + w.field,
		})
	}
	return limits, nil
}

// claudeUsagePolled is when the usage endpoint was last read, so refreshClaudeUsage can hold itself to claudeUsagePoll however often it is called.
var claudeUsagePolled struct {
	sync.Mutex
	at time.Time
}

// refreshClaudeUsage reads the usage endpoint and records what it says, at most once every claudeUsagePoll however often it is called. Input: a context, the HTTP client, the endpoint, the credentials file, and where to record the reading. Output: none — a failure is logged with what went wrong and nothing else, and leaves the last good reading standing.
func refreshClaudeUsage(ctx context.Context, client *http.Client, url, credsPath string, rec UsageRecorder) {
	claudeUsagePolled.Lock()
	if time.Since(claudeUsagePolled.at) < claudeUsagePoll {
		claudeUsagePolled.Unlock()
		return
	}
	claudeUsagePolled.at = time.Now()
	claudeUsagePolled.Unlock()

	ctx, cancel := context.WithTimeout(ctx, claudeUsageTimeout)
	defer cancel()
	limits, err := claudeUsage(ctx, client, url, credsPath)
	if errors.Is(err, ErrLoggedOut) && rec != nil {
		rec.RecordSignedOut(ProviderClaude, "the Claude login was refused: run claude login to sign in again")
		return
	}
	if err != nil {
		slog.Debug("claude: could not read the subscription's usage windows", "error", err)
		return
	}
	if rec != nil && len(limits) > 0 {
		rec.Record(ProviderClaude, limits)
	}
}

// RefreshClaudeUsage reads the Claude subscription's allowance windows into the recorder set by SetUsageRecorder, at most once every ten minutes. GET /brains calls it, so the endpoint is only ever read while someone is looking at the picker. Input: a context. Output: none.
func RefreshClaudeUsage(ctx context.Context) {
	usageRecorder.Lock()
	to := usageRecorder.to
	usageRecorder.Unlock()
	if to == nil {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	refreshClaudeUsage(ctx, http.DefaultClient, claudeUsageURL, ClaudeCredentialsPath(home), to)
}
