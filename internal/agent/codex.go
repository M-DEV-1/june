package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/genai"

	"june/internal/util"
)

// The direct Codex brain answers /ask with OpenAI models on the user's ChatGPT subscription by calling the Codex Responses backend with the tokens Codex CLI keeps in ~/.codex/auth.json, running June's own tools through the same gate and trace as the Gemini text path.
const (
	codexResponsesURL = "https://chatgpt.com/backend-api/codex/responses"
	codexTokenURL     = "https://auth.openai.com/oauth/token"
	codexClientID     = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexDefaultModel = "gpt-5.5"
	codexOriginator   = "codex_cli_rs"
	// codexClientVersion is the Codex CLI version June claims, because the backend gates which models an account may use on the codex_cli_rs/<version> user agent.
	codexClientVersion = "0.144.0"
	// codexAskTimeout bounds one whole ask, every tool round included, and is generous because a reasoning model can spend a minute on a single round.
	codexAskTimeout = 12 * time.Minute
	// codexRefreshWindow is how close to its expiry an access token may come before the next call refreshes it instead of waiting for a 401.
	codexRefreshWindow = 5 * time.Minute
	// codexDefaultRetryWait is how long an ask waits before its one retry when the backend refused with 429 and named no Retry-After of its own.
	codexDefaultRetryWait = 2 * time.Second
	// codexMaxRetryWait is the longest named delay an ask will sit through. Past this the rate limit is not a moment's pause but a spent allowance, and the user is better told so than left watching nothing happen.
	codexMaxRetryWait = 60 * time.Second
)

type codexClient struct {
	ResponsesURL string
	TokenURL     string
	AuthPath     string
	Model        string
	HTTP         *http.Client
	// InstallID identifies this installation to the backend, the way the Codex CLI sends its own installation id.
	InstallID string

	// refreshMu serialises refreshes so two asks that both see a 401 cannot spend the same one-time refresh token twice.
	refreshMu sync.Mutex
	mu        sync.Mutex
	tokens    *codexTokens
}

var (
	defaultCodexOnce sync.Once
	defaultCodex     *codexClient
)

// defaultCodexClient returns the process-wide client for the real backend, using JUNE_CODEX_MODEL when set and gpt-5.5 otherwise.
func defaultCodexClient() *codexClient {
	defaultCodexOnce.Do(func() {
		model := os.Getenv("JUNE_CODEX_MODEL")
		if model == "" {
			model = codexDefaultModel
		}
		// The client carries no overall timeout, which would cut a streaming answer off mid-sentence; the dial, handshake and header stages are bounded instead, and the ask's own deadline bounds the whole turn.
		transport := &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 90 * time.Second,
			ForceAttemptHTTP2:     true,
			MaxIdleConnsPerHost:   2,
		}
		defaultCodex = &codexClient{ResponsesURL: codexResponsesURL, TokenURL: codexTokenURL, AuthPath: codexAuthPath(), Model: model, HTTP: &http.Client{Transport: transport}, InstallID: codexInstallID()}
	})
	return defaultCodex
}

// codexLoggedIn reports whether the Codex CLI auth file exists and holds an access token, so a caller can decide whether Codex is worth trying.
func codexLoggedIn() bool {
	_, err := loadCodexAuth(defaultCodexClient().AuthPath)
	return err == nil
}

// actionToolNames are the tools that change something on the user's machine, so a turn that ran one is never replayed on another provider: the click or the keystroke would happen twice.
var actionToolNames = map[string]bool{"click": true, "type_text": true, "press_key": true, "click_at": true}

// actionHops counts the hops in a turn that changed something on screen, for the hand-over rule. Input: the turn's tool hops. Output: how many of them ran an action tool; looking, pointing and searching do not count, since repeating those changes nothing.
func actionHops(hops []ToolHop) int {
	n := 0
	for _, hop := range hops {
		if actionToolNames[hop.Name] {
			n++
		}
	}
	return n
}

// GeminiCannotAnswer reports whether err is a failure no Gemini model can fix — 503 because every model is overloaded, or 429 because the day's free-tier request allowance is spent — and so is worth handing to a different provider. Input: the error from a Gemini call, possibly wrapped. Output: true only for those two.
// It is the exported form of geminiCannotAnswer, for the background jobs in internal/brain that need the same rule the asks already use.
func GeminiCannotAnswer(err error) bool {
	return geminiCannotAnswer(err)
}

// geminiCannotAnswer reports whether an ask failed in a way no Gemini model can fix: 503 because every model is overloaded, or 429 because the day's free-tier quota is spent. Input: the error from askText, possibly wrapped. Output: true only for those two, since any other failure would fail the same way on any provider.
func geminiCannotAnswer(err error) bool {
	return apiErrorCode(err, 503) || apiErrorCode(err, 429)
}

// apiErrorCode reports whether err is, or wraps, an API error with the given status. The SDK returns the error by value; the pointer form is accepted too so a caller that wraps one is not missed.
func apiErrorCode(err error, code int) bool {
	var byValue genai.APIError
	var byPointer *genai.APIError
	return (errors.As(err, &byValue) && byValue.Code == code) || (errors.As(err, &byPointer) && byPointer.Code == code)
}

// round sends one Responses call and reads the stream back. Input: the system instruction, the full input so far, the tools to offer, the session key that ties every round of one ask together for the backend's prompt cache, and the callback parseCodexStream hands each drawn shape to as it arrives. A 401 is answered by one token refresh and one retry; any other non-2xx is a codexHTTPError.
func (c *codexClient) round(ctx context.Context, instructions string, input []any, tools []codexTool, session string, onShape func(itemID string, shape map[string]any)) (codexRound, error) {
	if input == nil {
		input = []any{}
	}
	body := map[string]any{
		"model":        c.Model,
		"instructions": instructions,
		"input":        input,
		"stream":       true,
		"store":        false,
		// The backend keeps nothing when store is false, so the model's own reasoning must come back encrypted and be echoed into the next round for it to remember what it was doing.
		"include":             []string{"reasoning.encrypted_content"},
		"reasoning":           map[string]any{"effort": "medium"},
		"parallel_tool_calls": false,
	}
	if key := codexPromptCacheKey(); key != "" {
		body["prompt_cache_key"] = key
	}
	if len(tools) > 0 {
		body["tools"] = tools
		body["tool_choice"] = "auto"
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return codexRound{}, fmt.Errorf("codex: encoding the request: %w", err)
	}
	sent, err := c.currentTokens()
	if err != nil {
		return codexRound{}, err
	}
	resp, err := c.send(ctx, payload, session)
	if err != nil {
		return codexRound{}, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		if err := c.refresh(ctx, sent.AccessToken); err != nil {
			// A refresh the endpoint refused is the moment a Codex login is known to be dead. Codex has no cheap pre-flight probe — its allowance rides the headers of real calls, and the only proof the login works is a refresh round-trip that spends and rotates the token — so this is the earliest anything can say so.
			if errors.Is(err, ErrLoggedOut) {
				recordSignedOut(ProviderCodex, "the Codex login was refused: run codex login to sign in again")
			}
			return codexRound{}, err
		}
		if resp, err = c.send(ctx, payload, session); err != nil {
			return codexRound{}, err
		}
	}
	defer resp.Body.Close()
	// Every response carries the account's allowance windows in its headers, the refusals included, so the picker's bars come from the calls June already makes rather than a call of their own.
	recordUsage(ProviderCodex, codexRateLimits(resp.Header, time.Now()))
	if resp.StatusCode/100 != 2 {
		wait, _ := util.ParseRetryAfter(resp.Header, time.Now())
		return codexRound{}, codexHTTPError{Code: resp.StatusCode, Body: util.BodySnippet(resp.Body), RetryAfter: wait}
	}
	return parseCodexStream(resp.Body, onShape)
}

// codexRetryWait says whether a failed round is worth one more try and how long to wait first. A 429 is the one failure that fixes itself given a moment, so it earns a single retry, in the same shape a 401 earns one refresh and one resend. Input: the error the round came back with and how many tools the turn has already run. Output: the wait and true only when the backend answered 429, no tool has run yet — a retry after a tool has run would take the user's action a second time, which is why the handover to another provider carries the same rule — and the delay is inside codexMaxRetryWait. A 429 that named no delay waits codexDefaultRetryWait.
func codexRetryWait(err error, hops int) (time.Duration, bool) {
	var httpErr codexHTTPError
	if err == nil || hops > 0 || !errors.As(err, &httpErr) || httpErr.Code != http.StatusTooManyRequests {
		return 0, false
	}
	wait := httpErr.RetryAfter
	if wait == 0 {
		wait = codexDefaultRetryWait
	}
	if wait > codexMaxRetryWait {
		return 0, false
	}
	return wait, true
}

// send posts the payload with the headers the Codex backend expects: the bearer token, the ChatGPT account id, the responses beta flag, and the Codex CLI originator, version and user agent, which together are the client identity the backend reads to decide which models the account may use.
func (c *codexClient) send(ctx context.Context, payload []byte, session string) (*http.Response, error) {
	tok, err := c.currentTokens()
	if err != nil {
		return nil, err
	}
	id := tok.accountID()
	if id == "" {
		return nil, errors.New("codex login: the auth file carries no ChatGPT account id; run `codex login` again")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ResponsesURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("chatgpt-account-id", id)
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("originator", codexOriginator)
	req.Header.Set("version", codexClientVersion)
	req.Header.Set("User-Agent", fmt.Sprintf("%s/%s (%s; %s)", codexOriginator, codexClientVersion, runtime.GOOS, runtime.GOARCH))
	if session != "" {
		req.Header.Set("session-id", session)
	}
	if c.InstallID != "" {
		req.Header.Set("x-codex-installation-id", c.InstallID)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codex: %w", err)
	}
	return resp, nil
}

// AskCodex answers a text question through the Codex backend with the user's ChatGPT login, running tools through the same read-only gate as the Gemini text path. Output: the turn trace with the answer, tool hops, evidence and model "codex/<model>", or the trace so far and an error.
func (a *Agent) AskCodex(ctx context.Context, question string) (TurnTrace, error) {
	return a.AskCodexWith(ctx, nil, question)
}

// AskCodexWith is AskCodex with the conversation so far sent ahead of the question, so a follow-up reads as one. Input: the prior turns (see HistoryFromTurns), nil for a question that stands alone, and the question. Output: the same TurnTrace AskCodex returns.
func (a *Agent) AskCodexWith(ctx context.Context, history History, question string) (TurnTrace, error) {
	return a.askCodex(ctx, defaultCodexClient(), history, question)
}

// frontFromToolHops reads the app-and-title text off the newest observe_screen look a turn has taken — the "app · title" line every observe_screen result opens with (see observeResult in tools.go) — which is what a screen round's personal context is trimmed against (see screenPersonalContext). Input: the turn's tool hops in call order. Output: that line, searched from the newest hop backwards, or "" when no observe_screen call has run yet or its result was an error.
func frontFromToolHops(hops []ToolHop) string {
	for i := len(hops) - 1; i >= 0; i-- {
		h := hops[i]
		if h.Name != "observe_screen" || strings.HasPrefix(h.Result, "error:") {
			continue
		}
		front, _, _ := strings.Cut(h.Result, "\n")
		return front
	}
	return ""
}

// codexHistoryItems renders the prior turns as Responses input items, the same message items a live turn uses, so the model reads the thread the way a person would. Input: the history, oldest first. Output: one item per turn in that order — the user's questions as {"role":"user"} messages with input_text content, the assistant's answers as {"role":"assistant"} messages with output_text content — and nil when there is no history.
func codexHistoryItems(history History) []any {
	var out []any
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
		role, partType := "assistant", "output_text"
		if c.Role == genai.RoleUser {
			role, partType = "user", "input_text"
		}
		out = append(out, map[string]any{"type": "message", "role": role, "content": []map[string]any{{"type": partType, "text": text.String()}}})
	}
	return out
}

// askCodex is AskCodexWith against the given client, so tests can point it at a fake backend.
func (a *Agent) askCodex(ctx context.Context, c *codexClient, history History, question string) (TurnTrace, error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, codexAskTimeout)
	defer cancel()
	// The look allowance, the picture draw maps coordinates against, and what the pictures cost all belong to one ask, carried on ctx from here on so a concurrent ask never shares this one's screenshot.
	ctx = withAskLookState(ctx)
	instruction := a.LeanPrompt(start)
	var handshake []string

	// Fetched again here, separately from the copy HandshakePrompt already rendered into instruction: a screen round drops instruction for screenTaskInstruction() and needs its own, trimmed personal context back (see screenPersonalContext), which the untrimmed handshake copy cannot supply.
	personalEntries, err := a.brain.PersonalContext(ctx)
	if err != nil {
		slog.Warn("askCodex: personal context fetch failed, continuing without it on screen rounds", "error", err)
	}

	recallCtx, cancelRecall := context.WithTimeout(ctx, recallTimeout)
	injected, err := a.brain.RetrieveRelevant(recallCtx, question, 2)
	cancelRecall()
	if err != nil {
		slog.Warn("AskCodex retrieve relevant failed, continuing without inject", "error", err)
		injected = nil
	}

	decls := a.askToolDeclarations()
	tools := codexTools(decls)
	screenTools := codexTools(trimToDeclarations(decls, screenRoundTools))
	// One key names this ask to the backend for the whole of it. What ties its rounds to the prompt cache is codexPromptCacheKey, which is the same on every ask.
	session := codexSessionKey()
	// The turn is assembled from three pieces every round rather than from one slice that only grows, because two of them shrink once the turn becomes a screen task: the thread is cut to its last turns and the instruction is swapped for the screen one.
	historyItems := codexHistoryItems(history)
	// The thread goes in ahead of this turn, oldest first, so the question the user is asking now is the last thing the model reads. The reference to a close past run sits between the two, with the turn rather than in the instruction: it is built from this question and the store, so it differs on every ask, and anything after it in the prompt could not be cached.
	turnParts := []map[string]any{{"type": "input_text", "text": turnContext(start, injected)}}
	reference, shownLessons := a.actReference(ctx, question, start)
	if reference != "" {
		turnParts = append(turnParts, map[string]any{"type": "input_text", "text": reference})
	}
	turnParts = append(turnParts, map[string]any{"type": "input_text", "text": question})
	turnItem := map[string]any{"type": "message", "role": "user", "content": turnParts}
	// trail is everything the rounds have added since: the model's own items echoed back and the tool outputs answering them.
	var trail []any
	// listings holds the tool outputs carrying a screen listing that is still being sent whole. Only the newest one is, so this never holds more than one entry.
	var listings []map[string]any
	tr := TurnTrace{
		Channel:   ChannelText,
		Model:     "codex/" + c.Model,
		Question:  question,
		Handshake: handshake,
		Injected:  injected,
		Usage:     TokenUsage{Provider: ProviderCodex},
		// Carried so the end-of-ask hook scores the lessons this run was shown.
		LessonsShown: shownLessons,
	}

	// A prompt cache matches a prefix, so every round of an ask that opens on different bytes than the round before it starts from nothing. Both of the pieces a screen round shrinks are therefore settled here, once, rather than rebuilt each round: the thread it keeps, and the instruction it sends. screenPrompt is left empty until the first round that actually needs it and then never changes, so a question whose own words already name a screen task (isScreenTask with no hops) has the same instruction on round 0 as on round 5, and a question that turns into a screen task partway through has the same one from that round on. The personal context it carries is trimmed against the first screen the ask saw, not against each round's own.
	screenThread := util.LastN(historyItems, maxScreenHistoryTurns)
	screenAsk := isScreenTask(question, nil)
	screenPrompt := ""
	// sameScreenAgain and onlyAnnotated (ask.go) decide which rounds count as one of the ask's maxAskIterations steps; maxAskRounds bounds a run that does nothing but repeat those.
	screen := ""
	// steps is how many rounds have actually spent one of the maxAskIterations steps, the same hard cap askText enforces.
	steps := 0
	for i := 0; i < maxAskRounds; i++ {
		prompt, thread, offered := instruction, historyItems, tools
		offeredNames := declNames(decls)
		if screenAsk || screenTaskStarted(tr.ToolHops) {
			if screenPrompt == "" {
				screenPrompt = screenTaskInstruction()
				if personal := screenPersonalContext(personalEntries, frontFromToolHops(tr.ToolHops)); personal != "" {
					screenPrompt = personal + "\n\n" + screenPrompt
				}
			}
			prompt, thread = screenPrompt, screenThread
			if screenRoundToolsSuffice(tr.ToolHops) {
				offered, offeredNames = screenTools, declNames(trimToDeclarations(decls, screenRoundTools))
			}
		}
		input := make([]any, 0, len(thread)+1+len(trail))
		input = append(input, thread...)
		input = append(input, turnItem)
		input = append(input, trail...)
		// Shapes drawn while the model was still writing the call they belong to, keyed by the stream item that carried it. Drawing marks the screen and changes nothing else, so doing it early costs nothing if the round then fails; the call itself draws only what is left (see streamDrawnFrom).
		early := map[string][]streamDrawn{}
		onShape := func(itemID string, shape map[string]any) {
			if a.Draw == nil {
				return
			}
			// The stream stops at maxDrawShapes: a call over the limit has its first maxDrawShapes shapes inked from here, and the finished call then refuses the rest through drawShapeList while still reporting these as drawn (see the early block in executeTool's draw case), so the model hears exactly what is on screen.
			if len(early[itemID]) >= maxDrawShapes {
				return
			}
			phrase, target, errText := a.drawOne(ctx, itemID, shape)
			if errText != "" {
				// Recorded rather than dropped: a shape refused during the stream still has to hold its place in early, or the shapes after it in the finished call shift onto the wrong entries (see streamDrawn.Err).
				early[itemID] = append(early[itemID], streamDrawn{Err: errText})
				return
			}
			early[itemID] = append(early[itemID], streamDrawn{Phrase: phrase, Target: target})
			// Logged because it is the only way to tell from outside whether the backend actually sends argument deltas: no line here means the drawing waited for the whole call, which is the same picture a little later rather than a failure.
			slog.Debug("ask codex: drew a shape off the stream", "item", itemID, "shape", phrase, "so_far", len(early[itemID]))
		}
		round, err := c.round(ctx, prompt, input, offered, session, onShape)
		if wait, ok := codexRetryWait(err, len(tr.ToolHops)); ok {
			slog.Warn("ask codex: the backend is rate limiting, waiting once and asking again", "wait", wait, "error", err)
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				tr.Duration = time.Since(start)
				return tr, fmt.Errorf("ask codex: round %d: %w", i, err)
			case <-timer.C:
			}
			// The failed attempt may have drawn some of a call before it died, and the retry sends the whole call again, so what it drew is forgotten rather than subtracted from the call that is about to arrive.
			clear(early)
			round, err = c.round(ctx, prompt, input, offered, session, onShape)
		}
		if err != nil {
			tr.Evidence = evidenceFromToolHops(tr.ToolHops)
			tr.Duration = time.Since(start)
			return tr, fmt.Errorf("ask codex: round %d: %w", i, err)
		}
		slog.Debug("ask codex: round", "iteration", i, "model", round.Model, "calls", len(round.Calls), "input_tokens", round.Usage.InputTokens, "output_tokens", round.Usage.OutputTokens, "cached_input_tokens", round.Usage.Details.CachedTokens, "instruction_bytes", len(prompt), "input_items", len(input))
		// Every round is counted, not only the last: a question answered after two tool calls cost what all three of its calls cost together.
		tr.Usage.Rounds++
		tr.Usage.add(round.Usage.InputTokens, round.Usage.OutputTokens, round.Usage.TotalTokens)
		tr.Usage.CachedInputTokens += round.Usage.Details.CachedTokens
		// Every finished item goes back exactly as it arrived, reasoning included, because the backend keeps nothing when store is false.
		for _, item := range round.Items {
			trail = append(trail, item)
		}
		if len(round.Calls) == 0 {
			// Only the round that ends without a tool call is the answer; text written beside tool calls is narration and must not reach the user.
			tr.Answer = strings.TrimSpace(round.Text)
			tr.Evidence = evidenceFromToolHops(tr.ToolHops)
			tr.ImageTokens = lookTokensSpent(ctx)
			tr.Duration = time.Since(start)
			return tr, nil
		}
		if t := strings.TrimSpace(round.Text); t != "" {
			tr.Narration = append(tr.Narration, t)
		}
		hopsBefore := len(tr.ToolHops)
		for _, call := range round.Calls {
			args := map[string]any{}
			var result string
			var parseFailed bool
			if call.Arguments != "" {
				if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
					parseFailed = true
					args = map[string]any{}
					raw := util.RunesEllipsis(call.Arguments, 200)
					slog.Warn("ask codex: tool call arguments did not parse", "tool", call.Name, "arguments", raw, "error", err)
					result = toolError(fmt.Sprintf("the arguments for %s did not parse as JSON: %s", call.Name, raw))
				}
			}
			ObserveTool(ctx, call.Name, toolActivitySummary(call.Name, args), false)
			if !parseFailed {
				result = a.evalExecute(WithOffered(withStreamDrawn(ctx, call.ID, early[call.ID]), offeredNames), call.Name, args)
			}
			ObserveTool(ctx, call.Name, resultSummary(call.Name, result), strings.HasPrefix(result, "error"))
			slog.Info("ask: tool", "tool", call.Name, "args", toolActivitySummary(call.Name, args), "result", resultSummary(call.Name, result), "detail", toolLogDetail(call.Name, result))
			tr.ToolHops = append(tr.ToolHops, ToolHop{Name: call.Name, Args: args, Result: result})
			// The output is paired to the call by call_id, falling back to the item id so a backend that sends only one of the two still gets a pairable answer.
			callID := call.CallID
			if callID == "" {
				callID = call.ID
			}
			output := map[string]any{"type": "function_call_output", "call_id": callID, "output": result}
			if fullScreenListing(call.Name, result) {
				listings = replaceSupersededCodexListings(listings, output)
			}
			trail = append(trail, output)
			// A function_call_output carries a string, so the picture goes in behind it as a user message of its own — the Responses API takes an image as an input_image content item with a data URL.
			if shot, ok := takeLook(ctx); ok {
				trail = append(trail, map[string]any{"type": "message", "role": "user", "content": []map[string]any{{
					"type":      "input_image",
					"image_url": "data:" + shot.Mime + ";base64," + base64.StdEncoding.EncodeToString(shot.Data),
				}}})
			}
		}
		if round := tr.ToolHops[hopsBefore:]; !sameScreenAgain(&screen, round) && !onlyAnnotated(round) {
			steps++
		}
		if steps >= maxAskIterations {
			break
		}
	}
	tr.Evidence = evidenceFromToolHops(tr.ToolHops)
	tr.ImageTokens = lookTokensSpent(ctx)
	tr.Duration = time.Since(start)
	return tr, capError(tr.ToolHops)
}

// replaceSupersededCodexListings blanks out every screen listing a newer one has replaced, in place, so a round sends the newest list whole and one short note for each older one. Input: the listing outputs still being sent whole, oldest first, and the output carrying the listing that has just arrived. Output: the outputs still being sent whole, which is the new one alone.
// The item is rewritten rather than dropped because it has to stay paired by call_id with the call the model made; what changes is only how much of it is spelled out.
func replaceSupersededCodexListings(older []map[string]any, newest map[string]any) []map[string]any {
	for _, item := range older {
		item["output"] = supersededListingNote
	}
	return append(older[:0], newest)
}

// CodexBrain is the asker the daemon registers under the "codex" brain name; it answers through the agent's AskCodex.
type CodexBrain struct {
	Agent *Agent
}

// AskText answers the question through AskCodex, so the ipc server can route a "codex" ask like any other brain.
func (b CodexBrain) AskText(ctx context.Context, question string) (TurnTrace, error) {
	return b.Agent.AskCodex(ctx, question)
}

// AskTextWith answers the question with the conversation so far, so the ipc server can hand a "codex" ask its thread exactly as it hands one to the default brain. Input: the prior turns and the question. Output: the turn trace from AskCodexWith.
func (b CodexBrain) AskTextWith(ctx context.Context, history History, question string) (TurnTrace, error) {
	return b.Agent.AskCodexWith(ctx, history, question)
}

// UsageLimit is one allowance window a provider reports for the user's own account: which window it is, how much of it is spent, when it resets, and where the reading came from.
// It is declared in this package rather than in internal/brain, where the store that keeps it lives, because internal/brain imports this package (see internal/brain/codex.go) and so this package cannot import it back; internal/brain aliases this type.
type UsageLimit struct {
	// Window names the allowance: "5h", "daily", "weekly", "monthly", or the provider's own name for it.
	Window string `json:"window"`
	// UsedFraction is how much of the window is spent, 0 to 1, so the window can draw it as a bar without knowing the provider's units.
	UsedFraction float64 `json:"used_fraction"`
	// ResetsAt is when the window starts again, and is the zero time when the provider named no reset.
	ResetsAt time.Time `json:"resets_at"`
	// Source names the field or header the reading came from, so a number on screen can be traced back.
	Source string `json:"source"`
}

// UsageRecorder keeps the newest usage reading for one provider. internal/brain.UsageStore is the implementation, and the daemon hands one in at startup. Input to Record: the provider id ("codex", "claude"), and its windows; a call with no windows must leave the last good reading alone.
type UsageRecorder interface {
	Record(provider string, limits []UsageLimit)
	// RecordSignedOut marks a provider whose credential the provider itself refused, with the sentence saying what to do about it. It is the only reliable sign-out signal these CLIs have: each one holds a refresh token, so an expired access token in the login file is ordinary and says nothing.
	RecordSignedOut(provider, note string)
}

// usageRecorder is where every response's usage reading goes, guarded because asks run concurrently with the startup that sets it. Nil means nothing is recorded, which is what a test or a daemon that never called SetUsageRecorder gets.
var usageRecorder struct {
	sync.Mutex
	to UsageRecorder
}

// SetUsageRecorder tells this package where to record the usage windows providers report on the calls June already makes. Input: the store, or nil to record nothing. Output: none.
func SetUsageRecorder(r UsageRecorder) {
	usageRecorder.Lock()
	defer usageRecorder.Unlock()
	usageRecorder.to = r
}

// recordSignedOut tells the recorder a provider refused the login, so the picker can grey the row out before a question dies on it. Input: the provider id and the sentence saying what to do. Output: none.
func recordSignedOut(provider, note string) {
	usageRecorder.Lock()
	to := usageRecorder.to
	usageRecorder.Unlock()
	if to != nil {
		to.RecordSignedOut(provider, note)
	}
}

// recordUsage hands one provider's windows to the recorder, and does nothing when there is no recorder or no window to record. Input: the provider id and its windows. Output: none.
func recordUsage(provider string, limits []UsageLimit) {
	if len(limits) == 0 {
		return
	}
	usageRecorder.Lock()
	to := usageRecorder.to
	usageRecorder.Unlock()
	if to != nil {
		to.Record(provider, limits)
	}
}

// codexRateLimits reads the account's allowance windows off one Codex response's headers. The backend answers with its own x-codex-* family rather than the usual x-ratelimit-* one: x-codex-primary-used-percent, x-codex-primary-window-minutes and x-codex-primary-reset-at (older backends: x-codex-primary-reset-after-seconds), and the same three for secondary. The primary window is the five-hour one and the secondary the weekly one on a ChatGPT plan.
// Input: the response headers and the moment the response arrived, which a reset-after-seconds header counts forward from. Output: one UsageLimit per window the headers named, primary first, and none when they named neither.
func codexRateLimits(h http.Header, now time.Time) []UsageLimit {
	var limits []UsageLimit
	for _, prefix := range []string{"primary", "secondary"} {
		percent, err := strconv.ParseFloat(strings.TrimSpace(h.Get("x-codex-"+prefix+"-used-percent")), 64)
		if err != nil {
			continue
		}
		minutes, _ := strconv.Atoi(strings.TrimSpace(h.Get("x-codex-" + prefix + "-window-minutes")))
		var resets time.Time
		if at, err := strconv.ParseInt(strings.TrimSpace(h.Get("x-codex-"+prefix+"-reset-at")), 10, 64); err == nil && at > 0 {
			resets = time.Unix(at, 0).UTC()
		} else if after, err := strconv.ParseInt(strings.TrimSpace(h.Get("x-codex-"+prefix+"-reset-after-seconds")), 10, 64); err == nil && after > 0 {
			resets = now.Add(time.Duration(after) * time.Second)
		}
		// A window the headers give neither a length nor a reset for cannot be drawn honestly: its label falls back to "primary"/"secondary", which names no allowance, and its reset renders as the year 1. This backend sends exactly that for secondary. Nothing is better than a bar that reads as a real untouched allowance.
		if minutes <= 0 && resets.IsZero() {
			continue
		}
		limits = append(limits, UsageLimit{
			Window:       codexWindowName(minutes, prefix),
			UsedFraction: percent / 100,
			ResetsAt:     resets,
			Source:       "x-codex-" + prefix + "-*",
		})
	}
	return limits
}

// codexWindowName is the label a window of that length is drawn under. Input: the window in minutes as the backend reported it, 0 when it reported none, and the name to fall back to. Output: "1h" through "23h" for a window shorter than a day (299 minutes, the five-hour window, rounds up to "5h"), "daily" for one about a day, "weekly" for one about a week, "monthly" for anything longer, and the fallback when the length is unknown.
func codexWindowName(minutes int, fallback string) string {
	switch {
	case minutes <= 0:
		return fallback
	case minutes < 1380:
		return fmt.Sprintf("%dh", (minutes+59)/60)
	case minutes < 2880:
		return "daily"
	case minutes < 20160:
		return "weekly"
	default:
		return "monthly"
	}
}
