package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"runtime"
	"strings"
	"time"

	"june/internal/util"

	"june/internal/config"
	"june/internal/db"

	"google.golang.org/genai"
)

// Channel is which Gemini surface a companion turn used.
type Channel string

const (
	// ChannelText is Models.GenerateContent against config.TextModel.
	ChannelText Channel = "text"
	// ChannelVoice is the Live API against config.VoiceModel(), matching production voice (handshake frozen, no per-turn RetrieveRelevant).
	ChannelVoice Channel = "voice"
)

// maxAskIterations is the checkpoint on tool-call steps a single ask may spend, on every path that runs a tool loop (askText, askVoice, askCodex, and the Claude/Antigravity command-line paths via claudeToolServer). Past it the loop stops and answers with capError: where it got to, what it last did, and an offer to carry on, which a "continue" picks up from the thread. It is a checkpoint rather than a limit on the task, so a long task runs as several stretches with the user able to steer between them, and a loop that is going nowhere still stops. The three failed runs of 2026-09-16 to 09-23 stopped at fifty steps after 130 to 200 seconds, so a hundred fits well inside the twelve-minute wall clock every path allows. A round that only re-observed a screen already seen, or only drew on it, does not spend a step (see sameScreenAgain, onlyAnnotated).
const maxAskIterations = 100

// askWallClock is the one hard stop on askText's tool loop, alongside the step cap above: past this much wall time since the turn started, the request in flight fails on its own deadline and the loop returns rather than spinning forever. The Codex, Claude and Antigravity paths already wrap their own call in a timeout of the same size (codexAskTimeout, claudeAskTimeout, agyAskTimeout); this is askText's equivalent, since Gemini's own GenerateContent path had none. A package var, not a const, so a test can shrink it rather than waiting twelve minutes for the stop to prove itself.
var askWallClock = 12 * time.Minute

// ToolHop is one model-initiated tool call plus the string we sent back.
type ToolHop struct {
	Name   string
	Args   map[string]any
	Result string
}

// Evidence is one stored row an answer leaned on, so the user can see where a name or a fact came from instead of trusting a paraphrase. Kind/ID/Title/When name the row (see db.EvidenceSource); Excerpt is the line the tool actually showed the model.
type Evidence struct {
	Kind    string
	ID      int64
	Title   string
	When    string
	Excerpt string
}

// TurnTrace is everything needed to study why a companion answer happened: frozen handshake, optional per-turn inject, thoughts, tool hops, the final reply, and the evidence (stored rows) the tool results carried.
type TurnTrace struct {
	Channel   Channel
	Model     string
	Question  string
	Handshake []string
	Injected  []string
	Thoughts  []string
	ToolHops  []ToolHop
	Evidence  []Evidence
	Answer    string
	Duration  time.Duration
	// Narration is the text the model wrote in rounds that also called tools, kept for the trace but never shown as the answer.
	Narration []string
	// Usage is what the turn cost in tokens, as the provider counted it, summed over every round of the tool loop.
	Usage TokenUsage
	// ImageTokens is what the pictures the look tool took cost this turn, estimated from their size (see lookTokenCost). It is part of Usage.InputTokens rather than extra to it: no provider breaks its input count down by part, so without this an ask that sent two screenfuls of pixels reads exactly like one that sent none.
	ImageTokens int
	// LastTarget is the screen item this turn's tools last acted on or pointed at (see tools.go's ScreenTarget), nil when none of them did. It reflects the agent's own running memory of the newest one (a.screenTarget), which is what a later ask's bare "it" is resolved against — see WithLastTargetHint.
	LastTarget *ScreenTarget
	// LessonsShown is the id of every lesson the reference block put in front of the model this turn (see RenderLessonBlock), for the end-of-ask hook (AfterScreenRun) to score once the run's outcome is known.
	LessonsShown []int64
}

// Provider names, as they are stored and as the screen that shows what each model provider costs groups by.
const (
	// ProviderGemini is the Gemini API, which serves both the text channel's GenerateContent calls and the voice channel's Live session.
	ProviderGemini = "gemini"
	// ProviderCodex is the Codex Responses backend, which serves OpenAI models on the user's ChatGPT login.
	ProviderCodex = "codex"
)

// TokenUsage is what one turn cost in tokens: the counts the provider itself reported, added up over every round of a multi-round tool loop, and the name of the provider that counted them. A provider that reports nothing leaves the counts at zero, so the call is still on record as having happened rather than carrying a guess.
type TokenUsage struct {
	Provider     string
	InputTokens  int
	OutputTokens int
	TotalTokens  int
	// Rounds is how many model calls the turn made. The counts above are already summed over them, so without this the ledger cannot tell a question answered in one call from one that spent twelve, which is most of the difference between a cheap ask and an expensive one.
	Rounds int
	// CachedInputTokens is how much of the input the provider answered out of its own prompt cache rather than reading afresh, as the provider reported it. It is part of InputTokens, not extra to it. A provider that reports none leaves it at zero.
	CachedInputTokens int
}

// add sums one round's counts into the turn's running total. Input: the input, output and total tokens that round reported. Output: none.
func (u *TokenUsage) add(input, output, total int) {
	u.InputTokens += input
	u.OutputTokens += output
	u.TotalTokens += total
}

// addCached sums one round's cached-prompt count into the turn's running total. Input: the count the provider reported, which is part of the input count and not extra to it. Output: none.
func (u *TokenUsage) addCached(cached int) {
	u.CachedInputTokens += cached
}

// addGemini sums one GenerateContent round's usage into the turn's running total. Input: the response's UsageMetadata, which is nil when the API reported none and then adds nothing. The prompt count is the input; the candidates count plus the thoughts count is the output, because thinking tokens are billed as output and are reported apart from the candidates; the cached-content count is the part of the prompt Gemini served out of its own implicit or explicit cache. Output: none.
func (u *TokenUsage) addGemini(md *genai.GenerateContentResponseUsageMetadata) {
	if md == nil {
		return
	}
	u.add(int(md.PromptTokenCount), int(md.CandidatesTokenCount)+int(md.ThoughtsTokenCount), int(md.TotalTokenCount))
	u.addCached(int(md.CachedContentTokenCount))
}

// addLive sums one Live API server message's usage into the turn's running total. Input: the message's UsageMetadata, which most messages of a turn do not carry and which then adds nothing. The Live API names its output the response count rather than the candidates count, and reports thoughts apart from it the same way the text channel does. Output: none.
func (u *TokenUsage) addLive(md *genai.UsageMetadata) {
	if md == nil {
		return
	}
	u.add(int(md.PromptTokenCount), int(md.ResponseTokenCount)+int(md.ThoughtsTokenCount), int(md.TotalTokenCount))
	u.addCached(int(md.CachedContentTokenCount))
}

// ToolObserver is called by askText's and askVoice's tool loops around each tool call, so a live caller can show progress while a multi-step turn is still running instead of learning about it only once the whole answer is back. Input: the tool's name and a short summary of what it is doing — its argument summary before the call runs, its result summary after — and failed, which is always false on the before-call and, on the after-call, true when the result string starts with "error" (the same convention ToolActivity.Err and resultSummary follow elsewhere). Output: none.
type ToolObserver func(name, summary string, failed bool)

// toolObserverKey is the unexported context key WithToolObserver stores under, so only this package's own toolObserverFrom can read it back.
type toolObserverKey struct{}

// WithToolObserver attaches fn to ctx so askText and askVoice report tool activity as it happens. Input: the parent context and the observer to call before and after each tool; fn may be nil. Output: a context carrying it.
func WithToolObserver(ctx context.Context, fn ToolObserver) context.Context {
	return context.WithValue(ctx, toolObserverKey{}, fn)
}

// toolObserverFrom reads the ToolObserver WithToolObserver attached to ctx. Output: that observer, or a no-op when ctx carries none (or carries a nil one).
func toolObserverFrom(ctx context.Context) ToolObserver {
	if fn, _ := ctx.Value(toolObserverKey{}).(ToolObserver); fn != nil {
		return fn
	}
	return func(string, string, bool) {}
}

// ObserveTool calls the ToolObserver attached to ctx, if any, with name, summary and failed. Exported (unlike toolObserverFrom itself) so a caller in another package — the ipc package's tests, standing in for what askText's real tool loop does around one call — can drive the same observer a live turn would report through, without needing the unexported context key.
func ObserveTool(ctx context.Context, name, summary string, failed bool) {
	toolObserverFrom(ctx)(name, summary, failed)
}

// questionKey is the unexported context key WithQuestion stores under, so only this package's own questionFrom can read it back.
type questionKey struct{}

// WithQuestion attaches the ask's own question text to ctx, so a tool such as click can tell whether the user actually asked to act on a window other than the one in front. Input: the parent context and the question. Output: a context carrying it.
func WithQuestion(ctx context.Context, question string) context.Context {
	return context.WithValue(ctx, questionKey{}, question)
}

// questionFrom reads the question WithQuestion attached to ctx. Output: that question, or "" when ctx carries none — which mentionsWindow reads as naming nothing.
func questionFrom(ctx context.Context) string {
	q, _ := ctx.Value(questionKey{}).(string)
	return q
}

// screenTargetPtr reads the agent's remembered screen target (see tools.go's screenTarget) as the pointer TurnTrace.LastTarget carries, nil when nothing has been pointed at or acted on yet.
func (a *Agent) screenTargetPtr() *ScreenTarget {
	if t, ok := a.screenTarget(); ok {
		return &t
	}
	return nil
}

// evidenceLimit caps how many Evidence entries a TurnTrace carries. A tool result can hold dozens of hits; the point of Evidence is showing the user the few rows an answer actually leaned on, not reproducing the whole search.
const evidenceLimit = 8

// The pattern assumes the tag holds no nested braces; a source title containing a literal { or } makes that tag unparseable and its evidence row is dropped, which loses a citation but never breaks the answer.
// sourceTagPattern matches the `{"source":{...}}` suffix db.FormatHitWithSource/FormatNoteHitWithSource append to a hit line — see withSourceTag in internal/db/search.go. No nested braces appear inside it, so a non-greedy match to the first "}}" is exact.
var sourceTagPattern = regexp.MustCompile(`\{"source":\{[^{}]*\}\}`)

// evidenceFromToolHops reads every source tag out of a turn's tool results, in the order the hits arrived (hop order, then line order within a hop — the same order the hits themselves ranked in), capped at evidenceLimit. A hop whose result carries no tag (shell_exec, save_note, a tool that failed) contributes nothing. Input: the tool hops a turn saw. Output: the Evidence entries, or nil when none of them carried a source tag.
func evidenceFromToolHops(hops []ToolHop) []Evidence {
	var out []Evidence
	for _, hop := range hops {
		// A hit can run to several lines (a note keeps its line breaks) with its tag on the last one, so the lines since the previous tag are the hit's own and go into its excerpt.
		var pending []string
		for _, line := range strings.Split(hop.Result, "\n") {
			loc := sourceTagPattern.FindStringIndex(line)
			if loc == nil {
				pending = append(pending, line)
				continue
			}
			excerpt := strings.TrimSpace(strings.Join(append(pending, line[:loc[0]]), "\n"))
			pending = nil
			var wrapped struct {
				Source db.EvidenceSource `json:"source"`
			}
			if err := json.Unmarshal([]byte(line[loc[0]:loc[1]]), &wrapped); err != nil {
				continue
			}
			out = append(out, Evidence{
				Kind:    wrapped.Source.Kind,
				ID:      wrapped.Source.ID,
				Title:   wrapped.Source.Title,
				When:    wrapped.Source.When,
				Excerpt: excerpt,
			})
			if len(out) >= evidenceLimit {
				return out
			}
		}
	}
	return out
}

// HandshakePrompt builds the exact frozen system instruction Connect() sends at Live handshake, plus the raw context lines it was assembled from. Input: ctx for GetImplicitContext / focus lookup; now is the date anchor. Output: full system-instruction string, and the indented memory lines that went into it.
func (a *Agent) HandshakePrompt(ctx context.Context, now time.Time) (instruction string, contextLines []string) {
	resp, err := a.brain.GetImplicitContext(ctx)
	if err != nil {
		slog.Warn("handshake context fetch failed, continuing without history", "error", err)
	}
	var contextParts []string
	for _, node := range resp {
		contextParts = append(contextParts, "  "+node)
	}
	contextParts = append(contextParts, a.surfacePendingFolds(ctx)...)
	contextParts = a.buildHandshakeContext(ctx, contextParts)

	tools := liveTools()
	toolsCount := 0
	if len(tools) > 0 {
		toolsCount = len(tools[0].FunctionDeclarations)
	}
	personalEntries, err := a.brain.PersonalContext(ctx)
	if err != nil {
		slog.Warn("handshake personal context fetch failed, continuing without it", "error", err)
	}
	return handshakeInstruction(now, personalContextBlock(personalEntries), strings.Join(contextParts, "\n"), toolsCount), contextParts
}

// LeanPrompt builds the system instruction a text ask sends: the same skeleton, tool guidance and stop line HandshakePrompt builds, but with no personal-context block and no retrieved-memory block. The user's decision behind it: only voice needs memory pushed ahead of time, because voice has to be proactive with no chance to reach for a tool mid-turn; a text ask can call query_memory, recall or personal_context when it actually needs a fact, so paying for the roughly nine thousand tokens of memory context on every question is waste the tools already recover for free. Input: the moment, for the clock line. Output: the instruction text.
func (a *Agent) LeanPrompt(now time.Time) string {
	tools := liveTools()
	toolsCount := 0
	if len(tools) > 0 {
		toolsCount = len(tools[0].FunctionDeclarations)
	}
	return systemInstructionStable(runtime.GOOS, runtime.GOARCH, shellName(), textCommunicationStyle, toolsCount) + "\n\n" + screenTaskGuidance + leanInstructionTail(now)
}

// textCommunicationStyle is the <communication_style> block for the text-ask paths (this file): unlike voiceCommunicationStyle, these replies are rendered as markdown in the chat pane (see app/src/next/chat-markdown.tsx), so the rules here are about how a written reply should read rather than how a spoken one should sound.
const textCommunicationStyle = `- Lead with the answer, then the reasons; every part of what they asked gets answered in the same reply, making them ask twice is friction, not brevity. No padding, never withheld substance: small talk gets a sentence, a real question gets the content, compact and complete.
- This reply is read on a screen, not spoken, so structure is allowed but it has to earn its place. Prose is the default. A list is for three or more parallel items a reader would scan (steps, options, findings), never for one point or a line of argument. A table is for comparing the same fields across several rows. A code block is for anything they would copy: a command, a path, an error, a snippet. A heading is for a reply long enough to need finding your way around, roughly over three hundred words, and never for a short one.
- Plain words. Say what happened, the number, and what it means; cut any sentence whose shape does the work the fact should do: no punchy fragments for effect, no metaphor standing in for a figure, no "not just X, it is Y", no tricolons, no remarks about the reply itself.
- Memory tools hand you raw captures: window titles, spreadsheet columns, terminal text. That is evidence, not your answer: no file path, extension, process name, URL, timestamp or stored label unless they asked for it or need it to act; say it the way they would. Give action items, findings and dates together in the order they matter, not in the order the tool returned them.
- A memory lookup takes under a tenth of a second, never announce it: no "let me check", no "one moment", just call the tool and answer. Mention only what will actually take time, a shell command, a large file read, a search already flagged as still running, in one short line.
- Whatever you find, say it. An empty result is an answer: "nothing in there about that, want me to look somewhere else?" A partial one is too: say what you actually got. No meta-acknowledgements ("got it", "sure", "noted") and no narrating your own process.
- Reply in the language they used, and keep that language to the end of the reply.`

// leanInstructionTail is systemInstructionTail with the personal-context and retrieved-memory blocks left out, keeping only the clock a text ask still needs to resolve "yesterday" or "this morning" for itself. Input: the moment. Output: the tail text.
func leanInstructionTail(now time.Time) string {
	return fmt.Sprintf("\n\nRight now it is %s, use this as your anchor for anything time-related (\"yesterday\", \"this morning\"); convert the period they mean into concrete since/until dates yourself.", nowAnchor(now))
}

// handshakeInstruction lays the ask paths' system instruction out for a prompt cache: everything that reads the same on every ask first (systemInstructionStable, then screenTaskGuidance), and only then the parts that change between asks (the personal block, the memory lines and the clock). Measured 2026-09-05 on Codex: with the changing parts in the middle, the cache matched 3,840 of a 10,300-token opening round; the one round where they happened not to change matched 9,984. Input: the moment, the personal context block ("" for none), the assembled context lines and the tool count. Output: the instruction text.
func handshakeInstruction(now time.Time, personal, contextStr string, toolsCount int) string {
	return systemInstructionStable(runtime.GOOS, runtime.GOARCH, shellName(), textCommunicationStyle, toolsCount) + "\n\n" + screenTaskGuidance + systemInstructionTail(now, personal, contextStr)
}

// screenTaskGuidance tells the ask paths (text and Codex; the live voice handshake in connect.go builds its own prompt and does not include this) that "go somewhere and do X" is a screen task to carry through, not a page to open and leave. Written from three 2026-09-05 traces: one opened a page and answered in nine words without ever looking at the screen, one clicked the nearest button and called a different thing done, and one opened a site's root over the page the user was already reading and spent the rest of its budget getting back. It is deliberately written about screens in general — lists, settings, threads, tables — because a rule that names one kind of site teaches the model nothing about the next one.
const screenTaskGuidance = `Doing something on the screen, going somewhere, starting something, changing a setting, is one task to see through, not a page to open and a sentence to say. Start from the window already in front: observe_screen first, and work in what's already open rather than opening a site's root over it. Stay in that window, never click a desktop overview, panel, dock or other app's window unless the request names another window; switch_window is the only tool that may bring a different one to front. When the target isn't in the current observe_screen list, scroll_to where it would be and observe_screen again, rather than clicking something else or stopping; narrow by section, group or filter first, then look for the item itself. Once it's in the list, click it there, then observe_screen again to confirm what opened or is playing is that same thing. Use the tool that performs the literal action asked for, not a shortcut to the same end state: scroll_to for scrolling, never a click; type_text for typing, always, open_url opens a page but types nothing. After every action, observe_screen again and check what actually changed. When what came up doesn't match, say so and keep working rather than calling it done; when something needed isn't there at all, say what's missing and what you'll try next.

Some of the screen is drawn, not laid out, video, a photo, a canvas, a map, a game, and none of it appears in the observe_screen list. To read it, or to point at or draw on it, call look first and take coordinates from the picture it returns. press_key is for keys no listing offers (Enter, Escape, Tab, Space) and lands wherever focus is, so click the field or player first. click_at and scroll_at are for elements the list has no entry or working action for, and only after a look, in that picture's coordinates.`

// stopLineText is the one safety rule that must survive every prompt this package trims: the sentence telling the model not to press anything irreversible without being told to. It is written out here rather than pulled from systemInstructionText because that prompt is a single long format string in connect.go with no seam to take one sentence out of, and TestStopLineText_IsTheSentenceTheHandshakeCarries fails the moment the two copies drift apart.
const stopLineText = `Never click anything that sends, pays, deletes or submits unless they have just said "go".`

// screenTaskInstruction is the system prompt a turn sends from the round after its first screen tool onwards. It is the screen-task guidance, the stop line, and one sentence saying who is talking — nothing else. The handshake an ask opens with teaches how to talk, what memory is for, and who the people in the user's life are; a round spent deciding which numbered button to press needs none of it, and on the 2026-09-05 runs it was about 7,800 of the roughly 9,800 input tokens every such round paid for. It takes no arguments on purpose: the same bytes on every screen round of every ask are what a prompt cache can match.
// Input: none. Output: the prompt text.
func screenTaskInstruction() string {
	return "You're June, working the user's screen for them. When done, say so in one or two plain spoken sentences, no markdown.\n\n" + screenTaskGuidance + "\n\n" + stopLineText
}

// screenTaskStarted reports whether a turn has committed to working on the screen, which is what lets an ask cut its prompt and its thread down to what a screen round actually needs. Input: the turn'"'"'s tool hops in call order. Output: true once any of them called a tool that looks at or acts on the screen.
func screenTaskStarted(hops []ToolHop) bool {
	for _, hop := range hops {
		if screenToolNames[hop.Name] || hop.Name == "draw" {
			return true
		}
	}
	return false
}

// supersededListingNote stands in for a screen listing that a later look has replaced. Only the newest observe_screen list is the one click, point_at and scroll_to resolve a number against, so an older listing is both dead weight and a trap: on 2026-09-05 a run pressed item 13 out of a list two looks old and landed in the desktop overview. Each listing costs roughly 1,300 tokens and used to be re-sent on every round after the one that produced it.
const supersededListingNote = "(an earlier look at the screen; its list is not shown any more and its numbers no longer resolve, only the newest observe_screen list counts)"

// fullScreenListing reports whether a tool result is a whole numbered screen listing rather than an error, a one-line "unchanged" answer, or some other tool'"'"'s output. Input: the tool'"'"'s name and the result string it returned. Output: true only for an observe_screen result that carries the numbered list itself.
func fullScreenListing(name, result string) bool {
	return name == "observe_screen" && !strings.HasPrefix(result, "error:") && strings.Contains(result, "\n[1] ")
}

// replaceSupersededListings blanks out every screen listing a newer one has replaced, in place, so a round sends the newest list whole and one short note for each older one. Input: the listing parts still being sent whole, oldest first, and the part carrying the listing that has just arrived. Output: the parts still being sent whole, which is the new one alone.
// The parts are rewritten rather than removed because the function response has to stay paired with the call the model made; what changes is only how much of it is spelled out.
func replaceSupersededListings(older []*genai.Part, newest *genai.Part) []*genai.Part {
	for _, part := range older {
		if part.FunctionResponse != nil {
			part.FunctionResponse.Response = map[string]any{"output": supersededListingNote}
		}
	}
	return append(older[:0], newest)
}

// screenRoundTools is the tool set a round of a screen task is offered: everything that looks at or acts on the screen, open_url, which 3 of the 53 recorded screen asks reached for after their first look, and save_note, so a screen task can still leave itself a note without regaining the rest of the memory surface. click_at is not among them any more: click itself now takes a bare x,y as well as a number, and a list of further taps for UI that closes before another round can see it, so one declaration covers what two used to. The whole list of 15 costs 3,091 tokens, measured against the backend on 2026-09-05, and it is re-sent on every round; these nine cost a good deal less than that, and the memory tools that go are of no use to a round deciding which numbered button to press.
// What this gives up, measured over the same 53 asks: one of them called query_memory after its first screen tool and would now have to answer without it. Putting a name back in this map is the whole of undoing that.
var screenRoundTools = map[string]bool{
	"look": true, "observe_screen": true, "point_at": true, "show_marks": true, "draw": true,
	"click": true, "scroll_to": true, "type_text": true, "open_url": true, "save_note": true, "wait_for": true,
	"press_key": true, "scroll_at": true, "open_app": true,
}

// screenTaskWordPattern matches a request naming a screen, a window, an app, a page, or an action that only makes sense done to one — clicking, typing, playing, opening, navigating — so a turn reads as a screen task from its very first round, before any tool has told it a screen is even involved. It is deliberately loose: a false match only costs the model tools it did not need this round, while a missed one is what let the 2026-09-05 runs reach for shell_exec, branch and query_memory on a request that plainly meant the screen, and spend rounds being refused.
var screenTaskWordPattern = regexp.MustCompile(`(?i)\b(screen|window|app|page|click\w*|typ(?:e|ing)\w*|play\w*|open\w*|navigat\w*)\b`)

// mentionsScreenTask reports whether the question itself names a screen task, by screenTaskWordPattern. Input: the question text. Output: true on a match.
func mentionsScreenTask(question string) bool {
	return screenTaskWordPattern.MatchString(question)
}

// isScreenTask reports whether a turn should be treated as a screen task for the purpose of which tools it is offered: either the question's own words name one, or a screen tool it has already called does (see screenTaskStarted). The question alone is what lets the very first round of "click the merge button" be narrowed, rather than waiting for a screen tool to run and prove it after the fact. Input: the question and the turn's tool hops so far. Output: true on either sign.
func isScreenTask(question string, hops []ToolHop) bool {
	return mentionsScreenTask(question) || screenTaskStarted(hops)
}

// screenRoundToolsSuffice reports whether a turn can be offered the short screen tool set. Input: the turn'"'"'s tool hops in call order. Output: false as soon as it has called anything the short list does not carry, because every call it made is echoed into every later round and an answer to a call the request no longer declares is one the backend can refuse.
func screenRoundToolsSuffice(hops []ToolHop) bool {
	for _, hop := range hops {
		if !screenRoundTools[hop.Name] {
			return false
		}
	}
	return true
}

// trimToDeclarations keeps only the declarations named in keep, in the order they were declared. Input: every declaration an ask offers and the set of names to keep. Output: the kept ones.
func trimToDeclarations(decls []*genai.FunctionDeclaration, keep map[string]bool) []*genai.FunctionDeclaration {
	out := make([]*genai.FunctionDeclaration, 0, len(keep))
	for _, d := range decls {
		if keep[d.Name] {
			out = append(out, d)
		}
	}
	return out
}

// trimToolsToScreen keeps only the tools a screen round needs, dropping any tool entry left with no declarations. Input: the tools an ask offers and the set of names to keep. Output: the trimmed tools.
func trimToolsToScreen(tools []*genai.Tool, keep map[string]bool) []*genai.Tool {
	out := make([]*genai.Tool, 0, len(tools))
	for _, tool := range tools {
		copyTool := *tool
		copyTool.FunctionDeclarations = trimToDeclarations(tool.FunctionDeclarations, keep)
		if len(copyTool.FunctionDeclarations) > 0 {
			out = append(out, &copyTool)
		}
	}
	return out
}

// maxScreenHistoryTurns is how much of the thread a screen round keeps: the last two turns, one question and the answer to it. "Do it again" and "the other one" reach back exactly that far; anything older describes a screen that has changed since, and the whole thread is re-sent on every round of the turn.
const maxScreenHistoryTurns = 2

// History is the conversation so far: the prior turns an ask sends ahead of the question, oldest first, in the same shape the multi-turn evals hand to AskWith. The Gemini text and voice paths send these Contents as they are; the Codex path renders the same turns into its own message items (see codexHistoryItems).
type History []*genai.Content

// The whole history is re-sent on every question of a thread, so an uncapped one both grows the request past what the model will take and charges for every earlier turn again on each new one — a ten-turn thread would pay for turn one ten times over. These three caps bound that.
const (
	// maxHistoryTurns keeps the newest 12 turns, six questions and their six answers. "Do it again" points at what was just said, and a hover thread that has run six exchanges is already further back than a follow-up reaches.
	maxHistoryTurns = 12
	// maxHistoryBytes caps the kept turns at 16 KB together, roughly four thousand tokens, so the thread never crowds out the handshake, the recalled memory lines and the tool results this question needs.
	maxHistoryBytes = 16000
	// maxHistoryTurnBytes truncates one turn longer than 4 KB instead of dropping it, because the newest answer is the one a follow-up refers to and losing its tail costs less than losing the whole of it.
	maxHistoryTurnBytes = 4000
)

// HistoryFromTurns builds the history an ask sends ahead of its question out of stored conversation rows. Input: the turns of one conversation, oldest first, as db.Store.ConversationTurns returns them. Output: the newest turns that fit the caps above, as user Contents for what was asked and model Contents for what was answered, in the same order.
// Only the text of each turn is carried: no tool names, no evidence, and no failed answer (kind "error", which is a message about the model rather than something it said). Past tool calls and their results are deliberately left out — a screen listing from a minute ago would tell the model the wrong thing about what is on screen now. A trailing question with no answer under it is the one being asked now, so it is dropped rather than sent twice.
func HistoryFromTurns(turns []db.Turn) History {
	kept := make([]*genai.Content, 0, len(turns))
	for _, t := range turns {
		if t.Kind == "error" {
			continue
		}
		text := strings.TrimSpace(t.Text)
		if text == "" {
			continue
		}
		role := genai.RoleModel
		if t.Role == "you" {
			role = genai.RoleUser
		}
		kept = append(kept, &genai.Content{Role: role, Parts: []*genai.Part{{Text: util.UTF8Bytes(text, maxHistoryTurnBytes)}}})
	}
	if len(kept) > 0 && kept[len(kept)-1].Role == genai.RoleUser {
		kept = kept[:len(kept)-1]
	}
	if len(kept) > maxHistoryTurns {
		kept = kept[len(kept)-maxHistoryTurns:]
	}
	// Measured from the newest turn backwards, so what a thread over the size cap loses is always its oldest part. The newest turn is kept whatever its size, since it has already been truncated to maxHistoryTurnBytes above.
	size, first := 0, len(kept)
	for i := len(kept) - 1; i >= 0; i-- {
		size += len(kept[i].Parts[0].Text)
		if size > maxHistoryBytes && i < len(kept)-1 {
			break
		}
		first = i
	}
	return History(kept[first:])
}

// AskText runs one question through GenerateContent (config.TextModel, then config.TextFallbackModel once if the first answers 503, then Codex on the user's ChatGPT login when that fails the same way) with the same handshake context Live gets, per-turn RetrieveRelevant like a typed turn, memory tools, and thoughts captured. Input: question text. Output: TurnTrace including thoughts and tool hops.
func (a *Agent) AskText(ctx context.Context, question string) (TurnTrace, error) {
	return a.AskTextWith(ctx, nil, question)
}

// AskTextWith is AskText with the conversation so far sent ahead of the question, so a follow-up like "do it again" reads as a follow-up instead of a question out of nowhere. Input: the prior turns (see HistoryFromTurns), nil for a question that stands alone, and the question. Output: the same TurnTrace AskText returns, including when the fallback model or Codex answers — both are given the same history.
func (a *Agent) AskTextWith(ctx context.Context, history History, question string) (TurnTrace, error) {
	return a.askRouted(ctx, Need{}, history, question)
}

// askRouted asks the providers the router picks for need, in its order, until one answers. Input: what the question requires, the conversation so far (nil for a question that stands alone) and the question. Output: the answering provider's trace, or the last error when none could answer.
func (a *Agent) askRouted(ctx context.Context, need Need, history History, question string) (TurnTrace, error) {
	// Which providers can answer, and in what order, is the router's decision (see router.go): it drops the ones this machine cannot run and the ones known to be out of allowance, then orders what is left by preference — Gemini first because it is cheapest, Claude last because it is also the user's coding workhorse. The fixed chain of ifs this replaces routed on liveness alone, so a question reached a provider that was merely alive rather than one that could answer it.
	return askInOrder(Route(need), func(id string) (TurnTrace, error) {
		switch id {
		case ProviderCodex:
			return a.AskCodexWith(ctx, history, question)
		case ProviderAgy:
			return a.AskAgyWith(ctx, history, question)
		case ProviderClaude:
			return a.AskClaudeWith(ctx, history, question)
		default:
			// Gemini alone has a second model to try before it counts as unable to answer, because a 503 means that one model is overloaded rather than that the provider is spent.
			tr, err := a.askText(ctx, config.TextModel, history, question)
			if shouldFallBack(err) {
				slog.Warn("ask text: model unavailable, asking the fallback", "model", config.TextModel, "fallback", config.TextFallbackModel, "error", err)
				tr, err = a.askText(ctx, config.TextFallbackModel, history, question)
			}
			return tr, err
		}
	})
}

// shouldFallBack reports whether an ask failed in the one way another model can fix: the API said 503 UNAVAILABLE, the model itself is overloaded. Input: the error from askText, possibly wrapped. Output: true only for a 503 API error.
func shouldFallBack(err error) bool {
	// The SDK returns APIError by value; the pointer form is accepted too so a caller that wraps one is not missed.
	var byValue genai.APIError
	var byPointer *genai.APIError
	return (errors.As(err, &byValue) && byValue.Code == 503) || (errors.As(err, &byPointer) && byPointer.Code == 503)
}

// AskVoice runs one question through the Live API (config.VoiceModel()) as a text client turn, matching production voice: handshake context only, no RetrieveRelevant injection. Input: question text. Output: TurnTrace from output transcription, thought parts, and tool hops.
func (a *Agent) AskVoice(ctx context.Context, question string) (TurnTrace, error) {
	return a.askVoice(ctx, config.VoiceModel(), nil, question)
}

// AskWith is AskText and AskVoice with the two things a multi-turn eval needs: the conversation so far, sent ahead of the question, and a model name that overrides the channel's default when it is not empty. Input: the channel, the model override ("" keeps the channel's own model), the prior turns as genai Contents in order, and the question. Output: the same TurnTrace the two single-question entry points return.
func (a *Agent) AskWith(ctx context.Context, ch Channel, model string, history []*genai.Content, question string) (TurnTrace, error) {
	switch ch {
	case ChannelText:
		if model == "" {
			model = config.TextModel
		}
		return a.askText(ctx, model, history, question)
	case ChannelVoice:
		if model == "" {
			model = config.VoiceModel()
		}
		return a.askVoice(ctx, model, history, question)
	}
	return TurnTrace{}, fmt.Errorf("ask: no such channel %q", ch)
}

// withHistory puts the turn in progress after the conversation so far, copying rather than appending in place so the caller can ask a second question against the same history. Input: the prior turns and the new turn's contents. Output: one slice holding both, in order.
func withHistory(history, turn []*genai.Content) []*genai.Content {
	out := make([]*genai.Content, 0, len(history)+len(turn))
	out = append(out, history...)
	return append(out, turn...)
}

// geminiBaseURL is the endpoint askText dials. Empty in production, which leaves the SDK on its own default; a test points it at a fake backend so a whole text ask, request body included, can be run without a network.
var geminiBaseURL string

func (a *Agent) askText(ctx context.Context, model string, history []*genai.Content, question string) (TurnTrace, error) {
	start := time.Now()
	now := start
	// One of the two hard stops on this loop, alongside maxAskIterations: no request past this deadline can go out. See askWallClock.
	ctx, cancel := context.WithTimeout(ctx, askWallClock)
	defer cancel()
	// Carried on ctx so a screen tool deep in the loop — click, checking whether the front window changed out from under it — can tell whether this very question named the window it now finds in front.
	ctx = WithQuestion(ctx, question)
	// The picture draw maps coordinates against and what the pictures cost belong to one ask, carried on ctx from here on so a concurrent ask never shares this one's screenshot.
	ctx = withAskLookState(ctx)
	instruction := a.LeanPrompt(now)
	var handshake []string

	recallCtx, cancel := context.WithTimeout(ctx, textSendLoopRetrieveTimeout)
	injected, err := a.brain.RetrieveRelevant(recallCtx, question, 2)
	cancel()
	if err != nil {
		slog.Warn("AskText retrieve relevant failed, continuing without inject", "error", err)
		injected = nil
	}

	// Built before the client so a call that failed before it sent anything still comes back naming the model and the provider it was going to, which is what the token ledger files it under.
	tr := TurnTrace{
		Channel:   ChannelText,
		Model:     model,
		Question:  question,
		Handshake: handshake,
		Injected:  injected,
		Usage:     TokenUsage{Provider: ProviderGemini},
	}

	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:      a.apiKey,
		Backend:     genai.BackendGeminiAPI,
		HTTPOptions: genai.HTTPOptions{BaseURL: geminiBaseURL},
	})
	if err != nil {
		return tr, fmt.Errorf("ask text: client: %w", err)
	}

	// The turn is assembled in three pieces rather than one growing slice, because two of them shrink once the turn turns into a screen task: the thread is cut to its last turns and the instruction is swapped for the screen one. The reference to a close past run goes in here, with the turn, since it is built from this question and the store and so differs on every ask — putting it in the instruction would be a different prompt prefix every time and nothing after it could be cached.
	turn, shownLessons := a.WithActReference(ctx, now, question, buildTurnContent(now, injected, question))
	tr.LessonsShown = shownLessons
	// A bare "it", "that" or "again" names nothing of its own — see the night of 2026-09-05, when "draw a circle around it" resolved against a fresh screen listing instead of the button turn 1 had just ringed. When this ask remembers a screen target and the question is one of those bare references, one sentence naming it goes in ahead of the question, the same way the act-reference block above does.
	if remembered, ok := a.screenTarget(); ok && isBareReference(question) && hintApplies(remembered, a.frontWindowNow(ctx)) {
		turn = WithLastTargetHint(remembered, turn)
	}
	var trail []*genai.Content
	tools := a.textAskTools()
	screenTools := trimToolsToScreen(tools, screenRoundTools)
	cfg := &genai.GenerateContentConfig{
		Tools:          tools,
		ThinkingConfig: thinkingConfig(),
	}

	// listings holds the function-response parts carrying a screen listing that is still being sent whole. Only the newest one is, so this never holds more than one entry.
	var listings []*genai.Part
	// Gemini serves a request out of its implicit cache only where the prefix is the same bytes, and only from 2,048 tokens of prefix upwards, so what a screen round narrows to is decided here, once, rather than each round: a question whose own words already name a screen task (isScreenTask with no hops) opens on the screen instruction, the cut thread and the screen tools at round 0 and keeps all three, and every round then only appends to what the round before it sent.
	screenThread := util.LastN(history, maxScreenHistoryTurns)
	screenAsk := isScreenTask(question, nil)
	screen := ""
	// steps is how many rounds of this loop have actually spent one of the ask's maxAskIterations tool-call steps (sameScreenAgain and onlyAnnotated below decide which rounds do not), so the loop can stop and hand back capError the moment the cap is hit rather than running on to maxAskRounds.
	steps := 0
	for i := 0; i < maxAskRounds; i++ {
		prompt, thread := instruction, history
		cfg.Tools = tools
		if screenAsk || screenTaskStarted(tr.ToolHops) {
			prompt, thread = screenTaskInstruction(), screenThread
			if screenRoundToolsSuffice(tr.ToolHops) {
				cfg.Tools = screenTools
			}
		}
		cfg.SystemInstruction = &genai.Content{Role: "system", Parts: []*genai.Part{{Text: prompt}}}
		// What this round was actually handed, which on a screen round is the short list — so a memory tool missing from a screen turn's record reads as the harness withholding it rather than the model passing it over.
		roundCtx := WithOffered(ctx, toolNames(cfg.Tools))
		contents := make([]*genai.Content, 0, len(thread)+len(turn)+len(trail))
		contents = append(contents, thread...)
		contents = append(contents, turn...)
		contents = append(contents, trail...)
		// Every round is one request against the day's allowance, so the gate is asked each time rather than once per ask.
		if err := a.allowGemini(model); err != nil {
			tr.Duration = time.Since(start)
			return tr, fmt.Errorf("ask text: generate (iteration %d): %w", i, err)
		}
		resp, err := client.Models.GenerateContent(ctx, model, contents, cfg)
		if err != nil {
			tr.Duration = time.Since(start)
			return tr, fmt.Errorf("ask text: generate (iteration %d): %w", i, err)
		}
		// Counted before the response is read, so a round that answered with tokens spent but no candidate still costs what it cost.
		tr.Usage.Rounds++
		tr.Usage.addGemini(resp.UsageMetadata)
		if len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil {
			tr.Duration = time.Since(start)
			return tr, fmt.Errorf("ask text: empty response (iteration %d)", i)
		}
		trail = append(trail, resp.Candidates[0].Content)
		text, thoughts := splitParts(resp.Candidates[0].Content.Parts)
		tr.Thoughts = append(tr.Thoughts, thoughts...)

		calls := resp.FunctionCalls()
		if len(calls) == 0 {
			// Only the round that ends without a tool call is the answer; text the model wrote beside its tool calls ("Saving a note for the user…") is narration and must not reach the user.
			tr.Answer = strings.TrimSpace(text)
			tr.Evidence = evidenceFromToolHops(tr.ToolHops)
			tr.LastTarget = a.screenTargetPtr()
			tr.ImageTokens = lookTokensSpent(ctx)
			tr.Duration = time.Since(start)
			return tr, nil
		}
		if t := strings.TrimSpace(text); t != "" {
			tr.Narration = append(tr.Narration, t)
		}
		var parts []*genai.Part
		// Pictures go in a user turn of their own after the tool responses: a function response carries a string, and the API takes an image as inline data on a plain user turn.
		var pictures []*genai.Part
		round := len(tr.ToolHops)
		for _, fc := range calls {
			ObserveTool(ctx, fc.Name, toolActivitySummary(fc.Name, fc.Args), false)
			result := a.evalExecute(roundCtx, fc.Name, fc.Args)
			ObserveTool(ctx, fc.Name, resultSummary(fc.Name, result), strings.HasPrefix(result, "error"))
			slog.Info("ask: tool", "tool", fc.Name, "args", toolActivitySummary(fc.Name, fc.Args), "result", resultSummary(fc.Name, result), "detail", toolLogDetail(fc.Name, result))
			tr.ToolHops = append(tr.ToolHops, ToolHop{Name: fc.Name, Args: fc.Args, Result: result})
			// The stop line already built its full answer — the refused action and the one-line question to unlock it — so the turn ends here on the model's own words rather than risking a further round that talks around the refusal or tries the click again a different way.
			if strings.HasPrefix(result, "Stopped before ") {
				tr.Answer = result
				tr.Evidence = evidenceFromToolHops(tr.ToolHops)
				tr.LastTarget = a.screenTargetPtr()
				tr.ImageTokens = lookTokensSpent(ctx)
				tr.Duration = time.Since(start)
				return tr, nil
			}
			part := genai.NewPartFromFunctionResponse(fc.Name, map[string]any{"output": result})
			if fullScreenListing(fc.Name, result) {
				listings = replaceSupersededListings(listings, part)
			}
			parts = append(parts, part)
			if shot, ok := takeLook(ctx); ok {
				pictures = append(pictures, &genai.Part{InlineData: &genai.Blob{MIMEType: shot.Mime, Data: shot.Data}})
			}
		}
		if round := tr.ToolHops[round:]; !sameScreenAgain(&screen, round) && !onlyAnnotated(round) {
			steps++
		}
		trail = append(trail, genai.NewContentFromParts(parts, genai.RoleUser))
		if len(pictures) > 0 {
			trail = append(trail, genai.NewContentFromParts(pictures, genai.RoleUser))
		}
		if steps >= maxAskIterations {
			break
		}
	}
	tr.ImageTokens = lookTokensSpent(ctx)
	tr.Duration = time.Since(start)
	tr.Evidence = evidenceFromToolHops(tr.ToolHops)
	tr.LastTarget = a.screenTargetPtr()
	return tr, capError(tr.ToolHops)
}

// maxAskRounds is the hard stop on how many times a tool loop may go round, whatever the step count says. A round that only looked at a screen it had already seen does not spend a step, so without this a model that does nothing but call observe_screen would loop forever. Twice the step cap: a task can afford one wasted look for every step it takes and still finish.
const maxAskRounds = 2 * maxAskIterations

// sameScreenAgain reports whether one round of tool calls did nothing but look at a screen the turn had already seen, so the cap can let that round pass without spending a step. Three observe_screen calls in a row that come back with the same window are one look at one screen, and counting them as three attempts is what left the 2026-09-05 run out of steps with the page half-done. Input: a pointer to the newest screen the turn has seen, which is updated in place whenever a look shows something new, and the tool hops of one round in call order. Output: true only when the round called at least one tool, every tool it called was observe_screen, and every result matched what was already on record.
func sameScreenAgain(seen *string, hops []ToolHop) bool {
	if len(hops) == 0 {
		return false
	}
	repeat := true
	for _, hop := range hops {
		if hop.Name != "observe_screen" {
			repeat = false
			continue
		}
		// A look that came back "unchanged" is by definition the screen already on record, and its text is not the listing, so it is matched by its marker rather than by comparing it with what was stored.
		if strings.Contains(hop.Result, unchangedScreenMarker) {
			continue
		}
		if hop.Result != *seen {
			repeat = false
			*seen = hop.Result
		}
	}
	return repeat
}

// annotationTools are the tools that mark the screen without changing it: they draw over what is showing, and nothing moves, opens or is pressed. Nothing they do has to be read back, so a round spent on them leaves the turn exactly where it was.
var annotationTools = map[string]bool{"draw": true, "point_at": true, "show_marks": true}

// onlyAnnotated reports whether one round of tool calls did nothing but mark the screen, so the cap can let that round pass without spending a step. It is the same reasoning sameScreenAgain applies to a look that saw nothing new: the step budget is sized for observe, act, observe again, and drawing is none of those. Marking up a diagram on 2026-09-05 drew ten shapes in ten rounds and so spent the whole twelve-step budget on drawing, ending in a cap error with the picture finished. Input: the tool hops of one round in call order. Output: true only when the round called at least one tool and every tool it called was an annotation.
func onlyAnnotated(hops []ToolHop) bool {
	if len(hops) == 0 {
		return false
	}
	for _, hop := range hops {
		if !annotationTools[hop.Name] {
			return false
		}
	}
	return true
}

// lastObservedWindow is the window the turn last saw, for the message an ask gives when it runs out of steps. Input: the turn's tool hops in call order. Output: the title from the newest observe_screen that worked — its first line, with the app name ahead of the separator dropped — or the whole first line when the window has no title, or "" when the turn never looked at the screen and every look failed.
func lastObservedWindow(hops []ToolHop) string {
	for i := len(hops) - 1; i >= 0; i-- {
		if hops[i].Name != "observe_screen" || strings.HasPrefix(hops[i].Result, "error:") {
			continue
		}
		line, _, _ := strings.Cut(hops[i].Result, "\n")
		app, title, ok := strings.Cut(line, " · ")
		if !ok {
			return strings.TrimSpace(line)
		}
		if strings.TrimSpace(title) != "" {
			return strings.TrimSpace(title)
		}
		return strings.TrimSpace(app)
	}
	return ""
}

// lastObservedApp is the app the run last looked at, which is where a lesson from it applies: a prompt spanning Spotify and WhatsApp ends in one of them. Input: the run's tool hops in call order. Output: the app half of the newest observe_screen that worked, or "" when the run never looked.
func lastObservedApp(hops []ToolHop) string {
	for i := len(hops) - 1; i >= 0; i-- {
		if hops[i].Name != "observe_screen" || strings.HasPrefix(hops[i].Result, "error:") {
			continue
		}
		line, _, _ := strings.Cut(hops[i].Result, "\n")
		app, _, _ := strings.Cut(line, " · ")
		return strings.TrimSpace(app)
	}
	return ""
}

// screenActionToolNames are the tools a screen task uses to do something rather than only look, for lastAction — observe_screen is left out on purpose, since seeing the screen again is not an answer to what the turn did on it.
var screenActionToolNames = map[string]bool{"click": true, "scroll_to": true, "type_text": true, "point_at": true, "draw": true, "show_marks": true, "press_key": true, "click_at": true, "scroll_at": true, "switch_window": true}

// lastAction describes the last thing a turn actually did to the screen, for the message it ends with when it runs out of steps — window and step count alone say where it stopped, not whether it had done anything there. Input: the turn's tool hops in call order. Output: the plain-words result of the newest screen-acting hop that did not fail, or "" when the turn never did anything but look, or never acted successfully at all.
func lastAction(hops []ToolHop) string {
	for i := len(hops) - 1; i >= 0; i-- {
		if !screenActionToolNames[hops[i].Name] || strings.HasPrefix(hops[i].Result, "error:") {
			continue
		}
		// A tool result tells the model what to do next after a "; " ("scrolled 8 steps; call observe_screen to see the page now"), and that half is not for the user.
		done, _, _ := strings.Cut(hops[i].Result, "; ")
		return strings.TrimSpace(done)
	}
	return ""
}

// StepCapError is what an ask returns when its tool loop spends every step. Msg is written for the user: where the turn got to, what it last did there, and an offer to carry on. The daemon files it as June's answer rather than as a failure, so a "continue" that follows has the task and where it stopped in its history.
type StepCapError struct{ Msg string }

func (e *StepCapError) Error() string { return e.Msg }

// capError builds the StepCapError an ask returns when it runs out of steps. Input: the turn's tool hops in call order. Output: the error naming the window it last saw and the last thing it did there, alongside how many tool calls it made; the window alone when it never acted; the count alone when it never even looked at a screen.
func capError(hops []ToolHop) error {
	window, action := lastObservedWindow(hops), lastAction(hops)
	var where string
	switch {
	case window != "" && action != "":
		where = fmt.Sprintf("I got as far as %q, having just %s, and ran out of steps after %d of them.", window, action, len(hops))
	case window != "":
		where = fmt.Sprintf("I got as far as %q and ran out of steps after %d of them.", window, len(hops))
	default:
		where = fmt.Sprintf("I ran out of steps after %d of them without settling on an answer.", len(hops))
	}
	return &StepCapError{Msg: where + " Say continue and I will carry on from there."}
}

func (a *Agent) askVoice(ctx context.Context, model string, history []*genai.Content, question string) (TurnTrace, error) {
	start := time.Now()
	now := start
	// See the matching line in askText: a screen tool later in this ctx's lifetime can read the question back off it.
	ctx = WithQuestion(ctx, question)
	// The look allowance and the picture draw maps coordinates against belong to this ask; see the matching line in askText. A voice turn never delivers the picture itself (see lookSeen), but draw still needs the state a look leaves behind to map a point back onto the screen.
	ctx = withAskLookState(ctx)
	instruction, handshake := a.HandshakePrompt(ctx, now)

	tr := TurnTrace{
		Channel:   ChannelVoice,
		Model:     model,
		Question:  question,
		Handshake: handshake,
		Usage:     TokenUsage{Provider: ProviderGemini},
	}

	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  a.apiKey,
		Backend: genai.BackendGeminiAPI,
		HTTPOptions: genai.HTTPOptions{
			APIVersion: "v1alpha",
		},
	})
	if err != nil {
		return tr, fmt.Errorf("ask voice: client: %w", err)
	}

	cfg := &genai.LiveConnectConfig{
		ResponseModalities: []genai.Modality{genai.ModalityAudio},
		SpeechConfig: &genai.SpeechConfig{
			VoiceConfig: &genai.VoiceConfig{
				PrebuiltVoiceConfig: &genai.PrebuiltVoiceConfig{
					VoiceName: config.DefaultVoice,
				},
			},
		},
		ThinkingConfig:           thinkingConfig(),
		InputAudioTranscription:  &genai.AudioTranscriptionConfig{},
		OutputAudioTranscription: &genai.AudioTranscriptionConfig{},
		Tools:                    a.askTools(),
		SystemInstruction: &genai.Content{
			Role:  "system",
			Parts: []*genai.Part{{Text: instruction}},
		},
	}

	if err := a.allowGemini(model); err != nil {
		return tr, fmt.Errorf("ask voice: live connect: %w", err)
	}
	session, err := client.Live.Connect(ctx, model, cfg)
	if err != nil {
		return tr, fmt.Errorf("ask voice: live connect: %w", err)
	}
	defer session.Close()

	if err := session.SendClientContent(genai.LiveSendClientContentParameters{
		Turns: withHistory(history, buildTurnContent(now, nil, question)),
	}); err != nil {
		tr.Duration = time.Since(start)
		return tr, fmt.Errorf("ask voice: send: %w", err)
	}

	var answer strings.Builder
	// toolRounds also stands in for how many model turns the session produced, since a Live turn that ran tools resumes once per tool round; Rounds is set from it wherever this function hands the trace back.
	toolRounds, spent, screen := 0, 0, ""
	for {
		if ctx.Err() != nil {
			tr.Answer = strings.TrimSpace(answer.String())
			tr.Usage.Rounds, tr.Duration = toolRounds+1, time.Since(start)
			if tr.Answer != "" {
				tr.Evidence = evidenceFromToolHops(tr.ToolHops)
				tr.LastTarget = a.screenTargetPtr()
				return tr, nil
			}
			return tr, ctx.Err()
		}
		msg, err := session.Receive()
		if err != nil {
			tr.Answer = strings.TrimSpace(answer.String())
			tr.Usage.Rounds, tr.Duration = toolRounds+1, time.Since(start)
			if tr.Answer != "" {
				tr.Evidence = evidenceFromToolHops(tr.ToolHops)
				tr.LastTarget = a.screenTargetPtr()
				return tr, nil
			}
			return tr, fmt.Errorf("ask voice: receive: %w", err)
		}
		// The Live API reports usage on the server message that finishes a response rather than on a response object, so a turn that ran tools reports one per round and they add up the way the text channel's rounds do.
		tr.Usage.addLive(msg.UsageMetadata)
		if msg.ServerContent != nil {
			if ot := msg.ServerContent.OutputTranscription; ot != nil && ot.Text != "" {
				answer.WriteString(ot.Text)
			}
			if msg.ServerContent.ModelTurn != nil {
				for _, part := range msg.ServerContent.ModelTurn.Parts {
					if part.Text == "" {
						continue
					}
					if part.Thought {
						tr.Thoughts = append(tr.Thoughts, part.Text)
					}
				}
			}
		}
		if msg.ToolCall != nil && len(msg.ToolCall.FunctionCalls) > 0 {
			toolRounds++
			if spent >= maxAskIterations || toolRounds > maxAskRounds {
				tr.Answer = strings.TrimSpace(answer.String())
				tr.Usage.Rounds, tr.Duration = toolRounds+1, time.Since(start)
				return tr, capError(tr.ToolHops)
			}
			var responses []*genai.FunctionResponse
			round := len(tr.ToolHops)
			for _, fc := range msg.ToolCall.FunctionCalls {
				ObserveTool(ctx, fc.Name, toolActivitySummary(fc.Name, fc.Args), false)
				result := a.evalExecute(WithOffered(ctx, toolNames(cfg.Tools)), fc.Name, fc.Args)
				ObserveTool(ctx, fc.Name, resultSummary(fc.Name, result), strings.HasPrefix(result, "error"))
				slog.Info("ask: tool", "tool", fc.Name, "args", toolActivitySummary(fc.Name, fc.Args), "result", resultSummary(fc.Name, result), "detail", toolLogDetail(fc.Name, result))
				tr.ToolHops = append(tr.ToolHops, ToolHop{Name: fc.Name, Args: fc.Args, Result: result})
				responses = append(responses, &genai.FunctionResponse{
					ID:       fc.ID,
					Name:     fc.Name,
					Response: map[string]any{"output": result},
				})
			}
			if round := tr.ToolHops[round:]; !sameScreenAgain(&screen, round) && !onlyAnnotated(round) {
				spent++
			}
			if err := session.SendToolResponse(genai.LiveSendToolResponseParameters{FunctionResponses: responses}); err != nil {
				tr.Answer = strings.TrimSpace(answer.String())
				tr.Usage.Rounds, tr.Duration = toolRounds+1, time.Since(start)
				return tr, fmt.Errorf("ask voice: tool response: %w", err)
			}
			continue
		}
		if msg.ServerContent != nil && (msg.ServerContent.TurnComplete || msg.ServerContent.GenerationComplete) {
			// A turn that only issued tool calls ends with its own TurnComplete before the tool response has been answered. Returning there reported an empty answer for every question that used a tool (seen 2026-09-03: eight of ten). Wait for the turn that carries speech.
			if toolRounds > 0 && strings.TrimSpace(answer.String()) == "" {
				continue
			}
			tr.Answer = strings.TrimSpace(answer.String())
			tr.Evidence = evidenceFromToolHops(tr.ToolHops)
			tr.LastTarget = a.screenTargetPtr()
			tr.Usage.Rounds, tr.Duration = toolRounds+1, time.Since(start)
			return tr, nil
		}
	}
}

// askTools is the tool list an ask offers the model: every tool the ask gate would actually run, and nothing else. A model cannot tell a tool it has been offered but may not use from one it may, so it spends a round finding out — the 2026-09-05 run that ran out of steps spent two of its twelve being refused shell_exec and then branch, both of which it had been handed. Input: none beyond the agent, whose evalWrites flag (see AllowEvalWrites) lifts the gate and so restores the full list. Output: the tools the gate admits, with any entry carrying no function declarations — Gemini's own search grounding, which has nothing to gate — kept as it is.
func (a *Agent) askTools() []*genai.Tool {
	// The same tool list the live session sends for this model. On the gemini-3 live models that list has no Google Search grounding, because pairing it with function tools closes the session with a quota error (measured 2026-09-02 and again here on 2026-09-03 when this still sent it).
	tools := liveTools()
	if a.evalWrites {
		return tools
	}
	out := make([]*genai.Tool, 0, len(tools))
	for _, tool := range tools {
		if len(tool.FunctionDeclarations) == 0 {
			out = append(out, tool)
			continue
		}
		copyTool := *tool
		copyTool.FunctionDeclarations = nil
		for _, d := range tool.FunctionDeclarations {
			if subtaskAllowedTools[d.Name] || askAllowedTools[d.Name] {
				copyTool.FunctionDeclarations = append(copyTool.FunctionDeclarations, d)
			}
		}
		if len(copyTool.FunctionDeclarations) > 0 {
			out = append(out, &copyTool)
		}
	}
	return out
}

// askToolDeclarations is askTools flattened to the function declarations alone, which is the shape the Codex path sends. Output: every declaration an ask offers, in the order the live session declares them.
func (a *Agent) askToolDeclarations() []*genai.FunctionDeclaration {
	var out []*genai.FunctionDeclaration
	for _, tool := range a.askTools() {
		out = append(out, tool.FunctionDeclarations...)
	}
	return out
}

// stripLiveOnlyFields copies each declaration with the NON_BLOCKING behavior cleared, which is what every caller of generateContent has to send. That flag is a Live API contract about when a tool result folds back into a spoken turn; generateContent has no such notion and rejects the whole request with "FunctionDeclaration.behavior is only supported by the BidiGenerateContent method". That 400 is what made every branch() call fail silently. Nothing else changes, so the text channel sees the same tool names, descriptions and schemas the voice channel does. Input: the declarations as the live session declares them; the originals are not modified. Output: a copy of each, in the same order, with the Live-only fields cleared; nil in gives nil out.
func stripLiveOnlyFields(decls []*genai.FunctionDeclaration) []*genai.FunctionDeclaration {
	if decls == nil {
		return nil
	}
	out := make([]*genai.FunctionDeclaration, 0, len(decls))
	for _, d := range decls {
		copyDecl := *d
		copyDecl.Behavior = ""
		out = append(out, &copyDecl)
	}
	return out
}

// textAskTools is askTools with the Live-only fields stripped off every declaration, so the text channel can send the same tools the voice channel declares (see stripLiveOnlyFields).
func (a *Agent) textAskTools() []*genai.Tool {
	tools := a.askTools()
	out := make([]*genai.Tool, 0, len(tools))
	for _, tool := range tools {
		copyTool := *tool
		copyTool.FunctionDeclarations = stripLiveOnlyFields(tool.FunctionDeclarations)
		out = append(out, &copyTool)
	}
	return out
}

// AllowEvalWrites lets AskText and AskVoice run every tool, not only the read-only memory set. For runs against a snapshot of the store, where a write cannot reach live data.
func (a *Agent) AllowEvalWrites() { a.evalWrites = true }

// askAllowedTools gates a text ask when AllowEvalWrites has not been called — which production /ask never does, so this is also the full list of tools a live user's turn may run. It is the subtask's read-only memory set, the screen tools (look, draw, click, scroll, type — none of them write to the store), and the store-writing tools that need no HITL approval: save_note, personal_context, revise, action_items, query_store, open_url. shell_exec, read_file and list_files stay out — they go through ToolApprovalChan, and nothing in the daemon reads that channel to answer the prompt, so a call would hang.
// screenToolNames are the tools whose results describe what is on the user's screen: a window title and the head of its accessibility list. That text can be a password manager, an inbox or a private chat, so it never goes into the log file even though the log is the user's own.
var screenToolNames = map[string]bool{"look": true, "observe_screen": true, "point_at": true, "show_marks": true, "click": true, "scroll_to": true, "type_text": true, "wait_for": true, "press_key": true, "click_at": true, "scroll_at": true, "switch_window": true}

// toolLogDetail is what the "ask: tool" log line carries as detail. Input: the tool's name and its full result. Output: "" for a screen tool, else up to the first 2048 bytes of the result, cut on a UTF-8 rune boundary.
func toolLogDetail(name, result string) string {
	// A screen tool's result is the front window's title and the head of its accessibility list, which can be a password manager or an inbox, so it stays out of the log; its failure carries none of that, only the reason, and the reason is what a log line saying "failed" was missing.
	if screenToolNames[name] && !strings.HasPrefix(result, "error") {
		return ""
	}
	// 2048 bytes matches the cap ToolRecord.Output keeps in the store, so the log line and the durable record never disagree about how much of a result is worth keeping; still capped so a huge file read cannot fill the log on its own.
	return util.UTF8Bytes(result, 2048)
}

var askAllowedTools = map[string]bool{
	"look": true, "observe_screen": true, "point_at": true, "show_marks": true, "draw": true, "click": true, "scroll_to": true, "type_text": true, "wait_for": true,
	"press_key": true, "click_at": true, "scroll_at": true, "open_app": true,
	"save_note": true, "add_task": true, "personal_context": true, "revise": true, "action_items": true, "query_store": true, "open_url": true,
	"delegate": true,
}

// evalExecute runs a tool for an ask, refusing the ones outside the gate. Input: the tool's name and arguments. Output: the tool's result, or a refusal naming the tool. The refusal used to read "disabled in evals (read-only memory eval)", which describes a situation a person typing a question is not in; askTools now keeps a gated tool off the list in the first place, so anything reaching this refusal is a model calling a tool it was never offered, and the message says the plain true thing instead.
func (a *Agent) evalExecute(ctx context.Context, name string, args map[string]any) string {
	if !a.evalWrites && !subtaskAllowedTools[name] && !askAllowedTools[name] {
		return fmt.Sprintf("error: tool %q %s", name, askGateMark)
	}
	return a.executeTool(ctx, name, args)
}

// ExecuteAskTool runs one tool exactly as an ask's own tool loop would: the same gate, the same stop line, the same screen tools. It is exported so a long-running computer-use job (internal/actjob) drives its steps through this one path rather than a second copy of the rules about what may be clicked and what may not. Input: the tool's name and arguments. Output: the tool's result, or the gate's refusal.
func (a *Agent) ExecuteAskTool(ctx context.Context, name string, args map[string]any) string {
	return a.evalExecute(ctx, name, args)
}

// lessonCapPerRun bounds how many lessons one screen run may write — automatic and reflective together — so one chatty run cannot flood an app's lessons in a single go.
const lessonCapPerRun = 2

// reflectiveLessonMinHops is the fewest tool hops a screen run must have made before the end-of-ask hook spends a model call asking what it would do differently. A run of one or two hops finished before there was anything to reflect on.
const reflectiveLessonMinHops = 3

// reflectivePromptFmt is the question the end-of-ask hook asks the same brain that just ran the turn, once per screen run that both had room left under lessonCapPerRun and did enough (see reflectiveLessonMinHops) to be worth asking about. In order: the app, the question the run was given, and what it actually did (reflectiveRunSummary).
//
// The run has to be in the question. Until 2026-09-12 this named only the app, and the call goes through AskText, which builds a fresh ask with a memory lookup of its own — so the model answered about whatever that lookup surfaced rather than about the run. That is how a run whose question was "open spotify, play classic rock playlist" filed a lesson about skipping a Spearman's correlation search: the run was never shown to it.
//
// It asks for one imperative line and a bare NONE otherwise, because the old wording ("Reply \"nothing\" if nothing") got "Nothing worth flagging — just browsing, a Meet call, and a PDF read" instead, nineteen times out of twenty-one. db.IsNothingLesson still guards the hedges, since a prompt cannot make a model obey.
const reflectivePromptFmt = `A screen run in %s has just finished.

It was asked: %q

What it did:
%s

Reply with one short imperative line a later run in this app could follow — a way of reaching something that worked, or a way that does not. Write about this run only. If there is nothing worth carrying forward, reply with the single word NONE and nothing else.`

// reflectiveHopCap is how many of a run's hops the reflective question shows. Twelve, the same count actReferenceStepCap holds a past run to: past that a run was feeling its way around rather than following a way that worked, and the tail is where that shows, so the last twelve are the ones kept.
const reflectiveHopCap = 12

// reflectiveResultRunes bounds each hop's result inside the summary. Enough for a whole error sentence ("error: there is no element 99 in the last observe_screen list") and for a click's own confirmation, short enough that twelve hops of a screen listing cannot turn one reflective call into a second full prompt.
const reflectiveResultRunes = 140

// reflectiveRunSummary renders what a run did, one hop per line, as "tool: result". Input: the run's hops in call order. Output: the last reflectiveHopCap of them with each result capped, or "" when there were none.
//
// The result is kept, unlike db.RenderActStep's plain-words rendering of the same hops, because a failure is the whole thing worth learning from — "error: there is no element 99" is the lesson, and a summary that says only "clicked item 99" hides it.
func reflectiveRunSummary(hops []ToolHop) string {
	if len(hops) == 0 {
		return ""
	}
	if len(hops) > reflectiveHopCap {
		hops = hops[len(hops)-reflectiveHopCap:]
	}
	lines := make([]string, 0, len(hops))
	for _, hop := range hops {
		result := strings.TrimSpace(util.RunesEllipsis(hop.Result, reflectiveResultRunes))
		if result == "" {
			result = "(no result)"
		}
		lines = append(lines, "- "+hop.Name+": "+result)
	}
	return strings.Join(lines, "\n")
}

// screenHopFailRe and screenHopOKRe recognise a click or scroll_to's own result wording — toolError's "could not click [%d] %s %q: %v" / "could not scroll to [%d] %s %q: %v" and the plain "clicked [%d] %s %q via %s" / "scrolled to [%d] %s %q" (tools.go) — so automaticLessons can read what failed and what later worked straight off a run's hops, with no model call.
var (
	screenHopFailRe = regexp.MustCompile(`^error: could not (?:click|scroll to) \[\d+\] (\S+) "([^"]*)": (.+)`)
	screenHopOKRe   = regexp.MustCompile(`^(?:clicked|scrolled to) \[\d+\] (\S+) "([^"]*)"(?: via (\w+))?`)
)

// screenElem identifies one screen element the way a run's own hops name it: its accessibility role and its label, the pair click and scroll_to both put in their result text. Item numbers are not part of it — observe_screen mints a fresh one on every look, so the same button can be [12] on one hop and [15] on the next.
type screenElem struct{ role, label string }

// automaticLessons finds, from a screen run's hops alone, every element the run failed on and then later succeeded on — a click or scroll_to whose result matched screenHopFailRe, followed later by one on the same role and label that matched screenHopOKRe — and writes each as one lesson line in the exact shape "<app>: <role> <label>: <what failed>; <what worked>". Input: the app the run happened in, its tool hops in call order, and the most lines to return. Output: up to cap lines, in the order their failures happened.
//
// ponytail: matches only on "the same element" (role+label), not "the same window" as well — every run this reads is already scoped to one app's front window, so the wider match the task allows would only ever widen it to the same pairs this already finds. Add the window check if a run ever spans more than one window.
func automaticLessons(app string, hops []ToolHop, maxLines int) []string {
	if maxLines <= 0 {
		return nil
	}
	var out []string
	failed := map[screenElem]string{}
	for _, hop := range hops {
		if hop.Name != "click" && hop.Name != "scroll_to" {
			continue
		}
		if m := screenHopFailRe.FindStringSubmatch(hop.Result); m != nil {
			failed[screenElem{m[1], m[2]}] = strings.TrimSpace(m[3])
			continue
		}
		m := screenHopOKRe.FindStringSubmatch(hop.Result)
		if m == nil {
			continue
		}
		elem := screenElem{m[1], m[2]}
		reason, hadFailed := failed[elem]
		if !hadFailed {
			continue
		}
		delete(failed, elem)
		worked := "it worked the next time"
		if m[3] != "" {
			worked = "worked via " + m[3]
		}
		out = append(out, fmt.Sprintf("%s: %s %q: %s; %s", app, elem.role, elem.label, reason, worked))
		if len(out) >= maxLines {
			break
		}
	}
	return out
}

// reflectiveLesson asks the same brain that just ran a screen turn, in one plain question through AskText — the cheapest existing entry point that gets a plain answer back without offering it screen tools it has no reason to reach for on a question about itself — what a later run in app should carry forward from this one. Input: ctx, the app, and the finished trace, whose question and hops go into the prompt so the model is reflecting on this run rather than on whatever its own memory lookup turned up. Output: the model's own words, trimmed, or "" when it said there was nothing, the call failed, app is "", or the run had no hops to describe.
func (a *Agent) reflectiveLesson(ctx context.Context, app string, trace TurnTrace) string {
	if app == "" {
		return ""
	}
	summary := reflectiveRunSummary(trace.ToolHops)
	if summary == "" {
		return ""
	}
	tr, err := a.AskText(ctx, fmt.Sprintf(reflectivePromptFmt, app, trace.Question, summary))
	if err != nil {
		slog.Warn("lessons: the reflective call failed, skipping", "error", err)
		return ""
	}
	reply := strings.TrimSpace(tr.Answer)
	if db.IsNothingLesson(reply) {
		return ""
	}
	return reply
}

// AfterScreenRun is the end-of-ask hook for a finished screen run. It first scores the lessons this run was shown (see RenderLessonBlock, TurnTrace.LessonsShown) against how the run ended, then writes up to lessonCapPerRun new ones: first the automatic ones a hop sequence alone proves (automaticLessons), then, if there is still room and the run did enough to be worth asking about (reflectiveLessonMinHops), one reflective line from the same brain that ran it. Input: ctx — the caller's own, not the ask's, since a finished turn must not be held up by this — the finished trace and its outcome ("ok" or "error"). Output: none; every failure is logged and swallowed, the same way recordActRun's own store writes are, because a lesson missed is not a turn failed.
func (a *Agent) AfterScreenRun(ctx context.Context, trace TurnTrace, outcome string) {
	store, ok := a.brain.(interface {
		ScoreLessonsUsed(ctx context.Context, ids []int64, outcome string) error
		AddLesson(ctx context.Context, app, goal, lesson string) (int64, error)
	})
	if !ok {
		return
	}
	if len(trace.LessonsShown) > 0 {
		if err := store.ScoreLessonsUsed(ctx, trace.LessonsShown, outcome); err != nil {
			slog.Warn("lessons: could not score the lessons this run was shown", "error", err)
		}
	}
	app := lastObservedApp(trace.ToolHops)
	if app == "" {
		return
	}
	lines := automaticLessons(app, trace.ToolHops, lessonCapPerRun)
	if len(lines) < lessonCapPerRun && len(trace.ToolHops) >= reflectiveLessonMinHops {
		if reflective := a.reflectiveLesson(ctx, app, trace); reflective != "" {
			lines = append(lines, reflective)
		}
	}
	for _, line := range lines {
		if _, err := store.AddLesson(ctx, app, trace.Question, line); err != nil {
			slog.Warn("lessons: could not store a lesson", "error", err)
		}
	}
}

// splitParts separates a model turn's parts into the text meant for the user and the thoughts the model marked as such. Input: the parts of one candidate. Output: the concatenated user-facing text and the thought texts in order.
func splitParts(parts []*genai.Part) (string, []string) {
	var text strings.Builder
	var thoughts []string
	for _, p := range parts {
		if p == nil || p.Text == "" {
			continue
		}
		if p.Thought {
			thoughts = append(thoughts, p.Text)
			continue
		}
		text.WriteString(p.Text)
	}
	return text.String(), thoughts
}
