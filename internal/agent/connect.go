package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"june/internal/config"
	"june/internal/obs"
	"june/internal/tracker"
	"june/internal/util"
	"log/slog"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/genai"
)

// controlTokenPattern matches the artifacts the Live API occasionally leaves in OutputTranscription text instead of consuming them internally: a literal "<ctrlN>" marker (a real session logged "june said" text of exactly "<ctrl46><ctrl46>"), or a run of the Unicode replacement character U+FFFD produced by the same kind of encoding slip.
var controlTokenPattern = regexp.MustCompile(`<ctrl\d+>|\x{FFFD}+`)

// stripControlTokens removes control-token artifacts from a chunk of June's own transcribed speech before it is logged or forwarded anywhere else. Applied at receiveLoop's single OutputTranscription choke point, upstream of both the "june said" log and the TUI/voice channel, so every downstream consumer sees the same cleaned text instead of each needing its own filter. Input: one OutputTranscription text chunk. Output: the chunk with control-token artifacts removed, otherwise untouched — spacing around real words is left alone since chunks stream in word-by-word and later get concatenated, so trimming here would merge two chunks together. A chunk that was nothing but such artifacts (or whitespace once they're gone) comes back empty.
func stripControlTokens(s string) string {
	cleaned := controlTokenPattern.ReplaceAllString(s, "")
	if strings.TrimSpace(cleaned) == "" {
		return ""
	}
	return cleaned
}

// prefixPaddingMs is the required duration of sustained detected speech before the Live API commits to "user started speaking". Raised above the SDK's zero-value default as a mitigation for acoustic echo (June's own voice bleeding into the mic and getting misread as a barge-in) — confirmed in production logs as 5+ false interrupts in under a minute. This is a tradeoff, not a fix: it also delays recognizing a genuine interruption by the same margin, and it does nothing for echo that outlasts the padding window. The real fix is acoustic echo cancellation (e.g. PulseAudio module-echo-cancel) or mic ducking during playback; this is a same-day mitigation pending those.
const prefixPaddingMs = 300

// bargeInConfirmWindow is how long an Interrupted that arrived with no user transcript is held open, waiting for one. The Live server raises Interrupted the moment its voice detector fires, and the matching InputTranscription fragment lands a few hundred milliseconds later, so requiring the transcript to already be there rejected real interruptions: 35 of 93 in the machine's log, 8 of them after echo cancellation was in place and the mic had stopped hearing June at all.
const bargeInConfirmWindow = 1200 * time.Millisecond

// bargeInEchoAmplitude is the speaker level above which June counts as audibly speaking. The speaker reports RMS scaled to [0,1] (see the audio package), where an idle stream sits at ~0 and speech runs 0.2-0.6, so this sits just above silence. Used to reject an interrupt that arrived with no user transcript while June's own voice was still playing — the room hearing itself, not someone cutting in.
const bargeInEchoAmplitude = 0.05

// realtimeInputConfig builds the Live API's voice-activity-detection config. Extracted from Connect() so it's testable without dialing a real websocket.
func realtimeInputConfig() *genai.RealtimeInputConfig {
	return &genai.RealtimeInputConfig{
		AutomaticActivityDetection: &genai.AutomaticActivityDetection{
			PrefixPaddingMs: genai.Ptr[int32](prefixPaddingMs),
		},
	}
}

// thinkingBudgetTokens bounds how many tokens the model may spend on internal reasoning per turn. Unset (the prior state) let a real session run multi-minute silent "thought" chains with no final reply — confirmed in production logs on 2026-08-09 (12 thought parts, 0 final replies, across a 5-minute session). Not zero: the observed thinking was genuinely useful synthesis (connecting a paper's argument to the user's own work), which is exactly the kind of help a voice companion should be able to give — this bounds runaway reasoning without disabling it.
// Cut from 1024 on 2026-08-28: the 2026-08-28 sessions measured roughly 1.8 seconds from the user's message to the first thought token, on turns whose whole job was one memory lookup and one sentence back. The dead-wait median across typed turns that day was 18.6 seconds, so this is not the whole problem, but it is the part that is paid on every single turn.
const thinkingBudgetTokens = 256

// thinkingConfig builds the Live API's thinking-behavior config for the configured voice model. Extracted from Connect() so it's testable without dialing a real websocket, same pattern as realtimeInputConfig.
func thinkingConfig() *genai.ThinkingConfig {
	return thinkingConfigFor(config.VoiceModel())
}

// thinkingConfigFor builds the thinking config the given Live model accepts. The Gemini 3 Live models take a thinking level and reject a token budget; the 2.5 model is the reverse. Low is the level that keeps first-word latency close to the 256-token budget the 2.5 model ran with, which on 2026-08-28 measured under two seconds for a lookup-and-answer turn.
// gemini-3.8-live is the exception on both counts: its reasoning is interleaved and it takes neither field, so it gets IncludeThoughts alone. Documented at ai.google.dev/gemini-api/docs/models/gemini-3.8-live on 2026-09-17, not probed — but sending a field the setup rejects is what takes the whole handshake down, so the safe reading of the doc is the one that sends less.
func thinkingConfigFor(model string) *genai.ThinkingConfig {
	cfg := &genai.ThinkingConfig{
		IncludeThoughts: true, // receiveLoop already routes Thought:true parts correctly (ResponseChunk.IsThought) — surfacing them costs nothing and helps diagnose exactly this class of issue.
	}
	if model == config.Live38Model {
		return cfg
	}
	if strings.HasPrefix(model, "gemini-3") {
		cfg.ThinkingLevel = genai.ThinkingLevelLow
		return cfg
	}
	cfg.ThinkingBudget = genai.Ptr[int32](thinkingBudgetTokens)
	return cfg
}

// compressionTriggerTokens is the context size at which the Live API compresses the session's history. At native audio's ~25 tokens/sec, 64k tokens is roughly the first 42 minutes of a session before compression ever kicks in. The model's own context window is 128k, so this sits at half of it, leaving headroom for the system prompt and tool schemas on top. Raised from an initial 25000 (the SDK's own suggested default) — if voice first-response latency degrades noticeably in live use, this and compressionTargetTokens are the knob to turn back down (toward 40000/16000, the previous step, before reverting all the way to 25000/8000).
const compressionTriggerTokens = 64000

// compressionTargetTokens is how far the Live API shrinks history back down once compressionTriggerTokens is hit. At ~25 tokens/sec of native audio, 32k tokens ≈ ~21 minutes of speech retained post-compression — doubled from an initial 8000 to cut down the in-session amnesia the user was hitting, at the cost of a larger per-turn token bill on every reply after a compression.
const compressionTargetTokens = 32000

// compressionConfig builds the Live API's context-window-compression config. Extracted from Connect() so it's testable without dialing a real websocket, same pattern as realtimeInputConfig/thinkingConfig. Without this, a session never trims its own context and eventually exhausts it (root cause of "voice breaks in longer conversations").
func compressionConfig() *genai.ContextWindowCompressionConfig {
	return &genai.ContextWindowCompressionConfig{
		TriggerTokens: genai.Ptr[int64](compressionTriggerTokens),
		SlidingWindow: &genai.SlidingWindow{TargetTokens: genai.Ptr[int64](compressionTargetTokens)},
	}
}

// proactivityConfig builds the Live API's proactive-audio config, or nil when the feature is switched off in june-config.json (see config.ProactiveAudioEnabled). Extracted from Connect() so it's testable without dialing a real websocket, same pattern as realtimeInputConfig/thinkingConfig/compressionConfig.
// Proactive audio lets the model decline to answer audio that wasn't aimed at it — a conversation in the room, a video playing, the user talking to someone else. June's mic is always open, so without it every stray sentence in earshot is a prompt. Only supported on the 2.5 native-audio models, which is what config.VoiceModel() is.
// Returning nil rather than a config with ProactiveAudio=false leaves the field off the wire entirely, so the API keeps its own default instead of June pinning it.
// affectiveDialogFor enables the 2.5 model's affective dialog, which matches the reply's tone to how the user sounds. Input: the Live model name. Output: true for the 2.5 native-audio model, nil for the gemini-3 live models, which reject the field.
func affectiveDialogFor(model string) *bool {
	if strings.HasPrefix(model, "gemini-3") {
		return nil
	}
	return genai.Ptr(true)
}

// turnClock measures how long the model takes to start speaking after the user stops. Input: the time of each transcribed fragment of user speech and the time of each audio chunk the model plays. Output: the gap from the last user fragment to the first audio chunk, reported once per model turn. The reference points are OpenAI's GPT-Live demo, measured 2026-09-03: about 0.1 s to a reply, 0.6 s to a one-word status when it delegates.
type turnClock struct {
	lastUser time.Time
	reported bool
}

func (c *turnClock) userSpoke(t time.Time) { c.lastUser = t }

func (c *turnClock) firstSound(t time.Time) (time.Duration, bool) {
	if c.reported || c.lastUser.IsZero() {
		return 0, false
	}
	c.reported = true
	return t.Sub(c.lastUser), true
}

func (c *turnClock) turnDone() { c.reported = false; c.lastUser = time.Time{} }

// ensureTurnID mints a fresh id for the voice session's current model turn if none is set yet, and returns it either way. Called at the first sign of a turn's content (receiveLoop's ModelTurn handling), so the first tool call and every one after it in the same turn share the same id. Input: none. Output: the turn's id.
func (a *Agent) ensureTurnID() string {
	a.turnMu.Lock()
	defer a.turnMu.Unlock()
	if a.turnID == "" {
		var buf [8]byte
		if _, err := rand.Read(buf[:]); err != nil {
			// A turn id is a debugging aid, not a correctness requirement, so a failed read falls back to the clock rather than losing the tool calls' record entirely.
			a.turnID = fmt.Sprintf("turn-%d", time.Now().UnixNano())
		} else {
			a.turnID = "turn-" + hex.EncodeToString(buf[:])
		}
	}
	return a.turnID
}

// currentTurnID reads back the current voice turn's id without minting one. Output: the id, or "" between turns and before the first one.
func (a *Agent) currentTurnID() string {
	a.turnMu.Lock()
	defer a.turnMu.Unlock()
	return a.turnID
}

// endTurn clears the voice turn's id at TurnComplete/GenerationComplete, so the next turn's ensureTurnID mints a fresh one instead of reusing the last.
func (a *Agent) endTurn() {
	a.turnMu.Lock()
	defer a.turnMu.Unlock()
	a.turnID = ""
}

// inputTranscriptionConfig asks for the text of what the user says. It sends no language hint: the Gemini API (as opposed to Vertex) rejects the languageCodes field at the handshake, which took the whole voice loop down on 2026-09-04 until a screenshot attempt found it. The hint had been added because on 2026-09-03 the server wrote English sentences in Devanagari; that has to be handled in the prompt instead.
func inputTranscriptionConfig() *genai.AudioTranscriptionConfig {
	return &genai.AudioTranscriptionConfig{}
}

func proactivityConfig(enabled bool) *genai.ProactivityConfig {
	return proactivityConfigFor(config.VoiceModel(), enabled)
}

// proactivityConfigFor is proactivityConfig for a named Live model. The 3.1 Live model does not support proactive audio as of 2026-09-02, so for it the field stays off the wire whatever the config file says — which also means the mic hears the room and the model answers it; see the trial note on config.VoiceModel().
// nil means the opposite thing for gemini-3.8-live: proactive audio is permanently enabled there and has no field to set, so the model always decides whether audio was aimed at it and the config file's switch cannot turn that off. Documented at ai.google.dev/gemini-api/docs/models/gemini-3.8-live on 2026-09-17, not probed.
func proactivityConfigFor(model string, enabled bool) *genai.ProactivityConfig {
	// The field is only accepted under API version v1alpha, which the live client sets; a probe on 2026-09-03 without it got `Unknown name "proactivity" at 'setup'`, so a client change here must keep v1alpha.
	if !enabled || strings.HasPrefix(model, "gemini-3") {
		return nil
	}
	return &genai.ProactivityConfig{ProactiveAudio: genai.Ptr(true)}
}

// nonSpeechMarker matches one bracketed sound description in an input transcript — "<noise>", "[laughter]", "(music)".
var nonSpeechMarker = regexp.MustCompile(`<[^<>]*>|\[[^\[\]]*\]|\([^()]*\)`)

// isNonSpeechTranscript reports whether an input transcript is nothing but bracketed sound markers, or empty. Such a transcript is the server describing a noise, not the user saying something, and on 2026-09-02 00:07 a bare "<noise>" that reached the model set off two memory lookups nobody had asked for.
func isNonSpeechTranscript(s string) bool {
	return strings.TrimSpace(nonSpeechMarker.ReplaceAllString(s, "")) == ""
}

// echoWindow is how long after June finishes saying something a matching transcript from the mic still counts as her own voice picked back up, not a new user turn. Set from the production loop that motivated this: the mic's transcript of June's own greeting arrived 4 seconds after she said it.
const echoWindow = 8 * time.Second

// echoHistorySize is how many of June's most recent utterances are kept for echo matching. A handful is enough to cover the case where she speaks two short things in quick succession before the mic's transcript of the first one comes back.
const echoHistorySize = 3

// juneUtterance is one thing June said, kept just long enough to catch the mic hearing it come back as if it were the user talking. Input to isEchoOfJuneSpeech: the text and the time she finished saying it — approximated as the moment the utterance was flushed, whether by a normal turn boundary or a barge-in.
type juneUtterance struct {
	text string
	end  time.Time
}

// wordPunctuation matches punctuation to strip when comparing two transcripts of the same speech, which can differ only in trailing punctuation or capitalization ("thing?" vs "thing").
var wordPunctuation = regexp.MustCompile(`[^\p{L}\p{N}\s]`)

// normalizeToWords lowercases a transcript, drops punctuation, and splits it into words. Input: one transcript. Output: its words, lowercased and stripped of punctuation, in order.
func normalizeToWords(s string) []string {
	return strings.Fields(wordPunctuation.ReplaceAllString(strings.ToLower(s), ""))
}

// commonSubsequenceLength returns the length of the longest common subsequence of two word slices: how many words of one appear in the other in the same relative order, not necessarily adjacent. Standard O(len(a)*len(b)) dynamic program — fine here since both inputs are single spoken utterances, at most a few dozen words.
func commonSubsequenceLength(a, b []string) int {
	dp := make([][]int, len(a)+1)
	for i := range dp {
		dp[i] = make([]int, len(b)+1)
	}
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			if a[i-1] == b[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else if dp[i-1][j] >= dp[i][j-1] {
				dp[i][j] = dp[i-1][j]
			} else {
				dp[i][j] = dp[i][j-1]
			}
		}
	}
	return dp[len(a)][len(b)]
}

// textsNearDuplicate reports whether two transcripts are close enough to be the same speech heard twice, case and trailing punctuation aside. Input: two transcripts. Output: true when one normalized text contains the other, or at least 80% of the shorter one's words appear in the same order within the longer one.
func textsNearDuplicate(a, b string) bool {
	wordsA, wordsB := normalizeToWords(a), normalizeToWords(b)
	if len(wordsA) == 0 || len(wordsB) == 0 {
		return false
	}
	joinedA, joinedB := strings.Join(wordsA, " "), strings.Join(wordsB, " ")
	if strings.Contains(joinedA, joinedB) || strings.Contains(joinedB, joinedA) {
		return true
	}
	shorter := len(wordsA)
	if len(wordsB) < shorter {
		shorter = len(wordsB)
	}
	return float64(commonSubsequenceLength(wordsA, wordsB))/float64(shorter) >= 0.8
}

// isEchoOfJuneSpeech reports whether a transcript the mic just picked up is June's own voice coming back through the speaker rather than something the user said. Input: what June said and when she finished, plus the candidate transcript and when it arrived. Output: true when the candidate arrived within echoWindow of June finishing and is a near-duplicate of what she said (see textsNearDuplicate).
func isEchoOfJuneSpeech(june juneUtterance, candidate string, arrived time.Time) bool {
	gap := arrived.Sub(june.end)
	if gap < 0 {
		gap = -gap
	}
	if gap > echoWindow {
		return false
	}
	return textsNearDuplicate(june.text, candidate)
}

// matchesRecentJuneSpeech reports whether candidate is an echo of any of June's recently kept utterances — see isEchoOfJuneSpeech. Shared by both call sites that need to tell a real user turn from the mic hearing June: the ordinary voice-transcript path and the barge-in path, so the rule lives in one place rather than being checked two different ways.
func matchesRecentJuneSpeech(recent []juneUtterance, candidate string, arrived time.Time) bool {
	for _, said := range recent {
		if isEchoOfJuneSpeech(said, candidate, arrived) {
			return true
		}
	}
	return false
}

func (a *Agent) Connect(ctx context.Context, micChan <-chan []byte) error {
	// Every tool call this session makes shares one screen state, the way every tool call of a typed ask shares the ask's. Without it each call got a throwaway: look recorded its picture into something discarded the moment it returned, and the draw that followed answered "I need to look at the screen first", so the model looked again and the two went round forever (a real session did exactly that on 2026-09-07).
	ctx = a.voiceScreenScope(ctx)

	tracer := obs.GetTracer(ctx, "june.agent")
	handshakeCtx, span := tracer.Start(ctx, "Agent.ConnectHandshake")

	client, err := genai.NewClient(handshakeCtx, &genai.ClientConfig{
		APIKey:  a.apiKey,
		Backend: genai.BackendGeminiAPI,
		HTTPOptions: genai.HTTPOptions{
			APIVersion: "v1alpha",
		},
	})
	if err != nil {
		span.RecordError(err)
		span.End()
		return fmt.Errorf("failed to initialize genai client: %w", err)
	}

	// drumrolllllll
	// connectttt to livee apiii
	span.SetAttributes(attribute.String("agent.model", config.VoiceModel()))

	_, ctxSpan := tracer.Start(handshakeCtx, "Agent.GetImplicitContext")
	resp, err := a.brain.GetImplicitContext(handshakeCtx)
	if err != nil {
		ctxSpan.RecordError(err)
		ctxSpan.SetStatus(codes.Error, "failed to fetch context")
		slog.Warn("context fetch failed, continuing without history", "error", err)
	}
	ctxSpan.End()

	var contextParts []string
	for _, node := range resp {
		contextParts = append(contextParts, "  "+node)
	}
	contextParts = append(contextParts, a.surfacePendingFolds(handshakeCtx)...)

	contextParts = a.buildHandshakeContext(handshakeCtx, contextParts)

	contextStr := strings.Join(contextParts, "\n")

	// Personal context is read whole, not searched — it is small by construction and every entry is something the user stated themselves.
	personalEntries, err := a.brain.PersonalContext(handshakeCtx)
	if err != nil {
		slog.Warn("personal context fetch failed, continuing without it", "error", err)
	}
	personal := personalContextBlock(personalEntries)

	tools := liveTools()
	toolsCount := 0
	if len(tools) > 0 {
		toolsCount = len(tools[0].FunctionDeclarations)
	}

	// config
	// Voice is configurable via /voice in the TUI (see config.AvailableVoices); falls back to config.DefaultVoice if unset or invalid.
	voiceName := a.GetVoice()
	if _, ok := config.NormalizeVoice(voiceName); voiceName == "" || !ok {
		voiceName = config.DefaultVoice
	}
	span.SetAttributes(attribute.String("agent.voice", voiceName))

	// priorHandle is whatever session-resumption handle a previous connection captured (see LiveServerSessionResumptionUpdate handling in receiveLoop).
	// Passing it lets the server resume the prior session instead of cold-starting; empty means a genuinely first-time connect, which also gates the opening-greeting send below so a resumed session doesn't repeat the greeting mid-conversation.
	priorHandle := a.getResumeHandle()
	span.SetAttributes(attribute.Bool("agent.session_resumed", priorHandle != ""))

	cfg := &genai.LiveConnectConfig{
		ResponseModalities: []genai.Modality{genai.ModalityAudio},
		SpeechConfig: &genai.SpeechConfig{
			VoiceConfig: &genai.VoiceConfig{
				PrebuiltVoiceConfig: &genai.PrebuiltVoiceConfig{
					VoiceName: voiceName,
				},
			},
		},
		// Requesting session resumption (even with an empty handle) makes the server send SessionResumptionUpdate messages so a handle can be captured for a future reconnect; a non-empty handle here resumes the previous session instead of starting cold.
		SessionResumption:   &genai.SessionResumptionConfig{Handle: priorHandle},
		RealtimeInputConfig: realtimeInputConfig(),
		ThinkingConfig:      thinkingConfig(),
		// InputAudioTranscription is what makes voice per-turn retrieval possible at all (see receiveLoop's InputTranscription handling below) — it does NOT change how audio is generated or played; it's purely an additive text side-channel alongside the native audio-in/audio-out the Live API already provides.
		InputAudioTranscription: inputTranscriptionConfig(),
		// OutputAudioTranscription is enabled for the same reason but has no consumer yet — June's own spoken replies transcribed to text is what conversation persistence needs, deliberately left for that later piece of work rather than half-wiring a consumer with nowhere to store the result.
		OutputAudioTranscription: &genai.AudioTranscriptionConfig{},
		Tools:                    tools,
		ContextWindowCompression: compressionConfig(),
		Proactivity:              proactivityConfig(config.LoadConfig().ProactiveAudioEnabled()),
		EnableAffectiveDialog:    affectiveDialogFor(config.VoiceModel()),
		SystemInstruction: &genai.Content{
			Role: "system",
			Parts: []*genai.Part{
				{Text: systemInstructionText(time.Now(), runtime.GOOS, runtime.GOARCH, shellName(), personal, contextStr, toolsCount)},
			},
		},
	}

	slog.Debug("connecting to live API")

	// A live session is one request against the day's allowance, asked for before the dial so a spent quota is reported without opening a socket.
	if err := a.allowGemini(config.VoiceModel()); err != nil {
		span.RecordError(err)
		span.End()
		return fmt.Errorf("live session refused: %w", err)
	}
	_, wsSpan := tracer.Start(handshakeCtx, "Agent.LiveConnectWebSocket")
	session, err := client.Live.Connect(handshakeCtx, config.VoiceModel(), cfg)
	if err != nil {
		wsSpan.RecordError(err)
		wsSpan.End()
		span.RecordError(err)
		span.End()
		return fmt.Errorf("websocket handshake failed: %w", err)
	}
	wsSpan.End()

	slog.Debug("connected to Live API")
	defer session.Close()

	// per-session context: cancels all goroutines for THIS session when Connect returns
	// w/o this, textSendLoop and audioSendLoop from a dead session linger across reconnects
	sessCtx, sessCancel := context.WithCancel(ctx)
	defer sessCancel()

	// kickstartttterrr
	// Only greet on a genuinely first-time connect (no stored resumption handle) — a resumed session is already mid-conversation from the server's point of view, and repeating the "just came online" greeting would talk over what the user already heard.
	if priorHandle == "" {
		a.writeMu.Lock()
		err = session.SendClientContent(genai.LiveSendClientContentParameters{
			Turns: []*genai.Content{
				{
					Role: "user",
					Parts: []*genai.Part{
						{Text: "(You've just come online and the user is here.) Lead with the greeting itself, spoken aloud — warm and brief, the way a friend who's been around all day would say hi. No preamble, no 'acknowledged', no narrating what you'll do — just the hello. Mention something real from their day only if it lands naturally."},
						// experiment prompt
					},
				},
			},
		})
		a.writeMu.Unlock()
		if err != nil {
			span.RecordError(err)
			slog.Error("failed to send initial client turn", "error", err)
		}
	} else {
		slog.Info("resuming live session, skipping opening greeting", "resumption_handle_present", true)
		// Anything a branch answered in the dead session's last seconds is not in the resumed context, so the model is told again rather than left saying the search is still running.
		a.redeliverBranchNotes(session)
	}

	// handshake complete, end span
	span.End()

	// Buffer 2: receiveLoop and audioSendLoop both write here — without room for both, the second writer blocks forever if Connect already returned on the first error.
	errChan := make(chan error, 2)

	go a.receiveLoop(sessCtx, session, a.GetModel(), errChan)
	go a.audioSendLoop(sessCtx, session, micChan, errChan)
	go a.textSendLoop(sessCtx, session)

	select {
	case err := <-errChan:
		return err
	case <-sessCtx.Done():
		return sessCtx.Err()
	case <-a.ReconnectChan:
		// e.g. /voice changed — the Live session's voice is fixed at handshake, so the only way to apply it is to drop and let the caller's reconnect loop redial with the new config.
		return fmt.Errorf("reconnecting to apply updated settings")
	}
}

// voiceScreenScope gives every tool call of the voice conversation the same screen state for as long as the agent lives, across reconnects. Input: the connection's context. Output: a context carrying the agent's own screen state.
// It used to be liveScreenScope, one fresh state per Connect. Measured 2026-09-09: the server hung up mid conversation, Connect redialled and resumed the same conversation from the model's point of view, and the model's next click on item 25 answered "call observe_screen first" about a list it had just been given. The model treats a resumed session as one conversation, so the numbered list, the picture and the focused control have to survive the redial.
func (a *Agent) voiceScreenScope(ctx context.Context) context.Context {
	return context.WithValue(ctx, askLookStateKey{}, &a.askScreen)
}

// ErrGoAway is what receiveLoop returns when it gave the session up because the server said it was about to hang up. The caller's reconnect loop redials on any error; this one says the drop was planned, not a failure.
var ErrGoAway = errors.New("live session ended on server GoAway")

// goAwayDrainLimit bounds how long a GoAway waits for June to finish the sentence she is speaking before the session is closed. The server gives its own deadline in GoAway.TimeLeft, which measured around ten seconds on 2026-09-09, so this stays well inside it.
const goAwayDrainLimit = 5 * time.Second

// goAwayDrainPoll is how often the speaker is asked whether it has gone quiet during that wait.
const goAwayDrainPoll = 100 * time.Millisecond

// waitForSpeakerQuiet blocks until the speaker is no longer audibly playing, or until limit runs out. Input: how long to wait at most. Output: none. A session with no speaker wired up (tests, text only) returns at once.
func (a *Agent) waitForSpeakerQuiet(limit time.Duration) {
	if a.speaker == nil {
		return
	}
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if a.speaker.CurrentAmplitude() <= bargeInEchoAmplitude {
			return
		}
		time.Sleep(goAwayDrainPoll)
	}
}

// branchNote is one branch result the model may never have seen: the task it was given, what came back, and when it was ready.
type branchNote struct {
	task   string
	result string
	at     time.Time
}

// branchNoteWindow is how long a branch result stays worth telling the model after a reconnect. Measured 2026-09-09: a branch result was sent at 03:43:41 and the server killed the socket at 03:43:50, nine seconds later, and the resumed session had no trace of it, so June kept saying the search was still running. Ninety seconds covers that gap and the redial after it.
const branchNoteWindow = 90 * time.Second

// pendingBranchNotes holds the recent branch results of each live agent, so a resumed session can be told the news the dead one carried. Notes past branchNoteWindow are dropped on every read and write, and an agent left with none is removed from the map, so nothing is held past the conversation it belongs to.
var pendingBranchNotes = struct {
	mu    sync.Mutex
	notes map[*Agent][]branchNote
}{notes: map[*Agent][]branchNote{}}

// rememberBranchResult keeps one branch result, so a reconnect within branchNoteWindow can hand it to the resumed session. Input: the task, the result string exactly as the tool produced it (an "error..." string is a failure), and the time it was ready. Output: none.
func (a *Agent) rememberBranchResult(task, result string, at time.Time) {
	pendingBranchNotes.mu.Lock()
	defer pendingBranchNotes.mu.Unlock()
	kept := freshBranchNotes(pendingBranchNotes.notes[a], at)
	pendingBranchNotes.notes[a] = append(kept, branchNote{task: task, result: result, at: at})
}

// takeBranchNotes returns this agent's branch results from the last branchNoteWindow and forgets them, so a second reconnect does not tell the model the same news twice. Input: now. Output: the notes, oldest first, or none.
func (a *Agent) takeBranchNotes(now time.Time) []branchNote {
	pendingBranchNotes.mu.Lock()
	defer pendingBranchNotes.mu.Unlock()
	notes := freshBranchNotes(pendingBranchNotes.notes[a], now)
	delete(pendingBranchNotes.notes, a)
	return notes
}

// freshBranchNotes drops the notes older than branchNoteWindow. Input: the notes and now. Output: the ones still worth delivering.
func freshBranchNotes(notes []branchNote, now time.Time) []branchNote {
	var kept []branchNote
	for _, n := range notes {
		if now.Sub(n.at) <= branchNoteWindow {
			kept = append(kept, n)
		}
	}
	return kept
}

// branchNoteText is what the model is told about one background search that finished while the socket was down. Input: the note. Output: one sentence, saying the result or the failure.
func branchNoteText(n branchNote) string {
	if strings.HasPrefix(n.result, "error") {
		return fmt.Sprintf("The background search for %s failed: %s", n.task, n.result)
	}
	return fmt.Sprintf("The background search for %s came back: %s", n.task, n.result)
}

// redeliverBranchNotes tells a resumed session about the branch results the previous connection was carrying when it died. Input: the new session. Output: none. Sent as client content text because a function response belongs to a call this new session never made.
func (a *Agent) redeliverBranchNotes(session liveSession) {
	for _, n := range a.takeBranchNotes(time.Now()) {
		a.writeMu.Lock()
		err := session.SendClientContent(genai.LiveSendClientContentParameters{
			Turns: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: branchNoteText(n)}}}},
		})
		a.writeMu.Unlock()
		if err != nil {
			slog.Error("failed to re-deliver a branch result to the resumed session", "task", n.task, "error", err)
		}
	}
}

// liveSession is the subset of *genai.Session's behavior receiveLoop depends on.
// Extracting it as an interface lets tests exercise receiveLoop's concurrency behavior (notably: a slow tool call must not block the receive path) against a fake session instead of a live websocket.
// *genai.Session satisfies this structurally already — no wrapping needed at call sites.
type liveSession interface {
	Receive() (*genai.LiveServerMessage, error)
	SendToolResponse(genai.LiveSendToolResponseParameters) error
	SendClientContent(genai.LiveSendClientContentParameters) error
	// SendRealtimeInput is the road the microphone already uses every frame, and the only one that can carry a picture: a tool result on this session is text, so a screenshot returned as one is never seen. The look tool pushes its capture down here instead (see sessionPictureSender), which is what lets the voice session read the screen at all.
	SendRealtimeInput(genai.LiveRealtimeInput) error
	// Close hangs up the websocket. receiveLoop calls it when the server sends a GoAway, so the client closes first rather than being force-closed with "close 1008 client failed to close the connection after receiving a GoAway" (measured three times in 25 minutes on 2026-09-09).
	Close() error
}

// sessionPictureSender is the road a look's picture takes on a live session. Input: the session. Output: a PictureSender that ships the capture as one realtime video frame.
// It goes in the Video field, the way the microphone's audio goes in Audio. The Media field serialises to media_chunks, which the API deprecated: measured 2026-09-09 at 03:58:40 and 04:04:33, a picture sent that way closed the socket with "close 1007 realtime_input.media_chunks is deprecated. Use audio, video, or text instead" and took the branch subtask running under it down with the session.
// It is realtime input rather than a tool result because the Live API takes a function response as text: the picture has to arrive on the same channel as the microphone's audio, and the tool result then only describes the frame it was taken in.
// It takes a.writeMu around the send, the same as every other write to the session (audioSendLoop, textSendLoop, runToolWithNudge's own nudge below): gorilla/websocket allows only one writer at a time, and a picture pushed without the lock can land mid-frame of a concurrent audio write and panic the connection with "concurrent write to websocket connection".
func (a *Agent) sessionPictureSender(session liveSession) PictureSender {
	return func(_ context.Context, c tracker.Capture) error {
		a.writeMu.Lock()
		defer a.writeMu.Unlock()
		return session.SendRealtimeInput(genai.LiveRealtimeInput{Video: &genai.Blob{Data: c.Data, MIMEType: c.Mime}})
	}
}

// VoiceUsage returns the Live API token usage of this session's most recently completed voice turn. Call it right after observing ResponseChunk{TurnBoundary: true} on TextResponseChan, and before the next turn completes — receiveLoop snapshots the running total on the agent and resets it for the next turn at the same TurnComplete/GenerationComplete point that flushes the turn's transcript buffers (see flushTurnUsage in receiveLoop). Output: that turn's usage, or the zero value TokenUsage{Provider: ProviderGemini} before any voice turn has completed on this session.
// The production voice session (Connect/receiveLoop, this file) has no TurnTrace to carry usage out in, unlike the eval voice path in ask.go: it runs for the whole lifetime of Connect(), not once per question, and the only thing that already leaves receiveLoop per turn is a ResponseChunk{TurnBoundary: true} on TextResponseChan. The usage therefore rests on the agent itself (see the voiceUsage field), which is also what keeps a finished session from leaving anything behind: a fresh agent is built for every voice session, so a package-level table keyed by the agent held every session the user ever opened alive for the life of the daemon.
func (a *Agent) VoiceUsage() TokenUsage {
	if u, ok := a.voiceUsage.Load().(TokenUsage); ok {
		return u
	}
	return TokenUsage{Provider: ProviderGemini}
}

// recordVoiceTurnUsage stores usage as this session's just-completed voice turn total, overwriting whatever the previous turn left there — see VoiceUsage.
func (a *Agent) recordVoiceTurnUsage(usage TokenUsage) {
	a.voiceUsage.Store(usage)
}

func (a *Agent) receiveLoop(ctx context.Context, session liveSession, model string, errChan chan error) {
	otelTracer := obs.GetTracer(ctx, "june.agent")
	recvCtx, recvSpan := otelTracer.Start(ctx, "Agent.ReceiveLoop")
	defer recvSpan.End()

	// inputTranscriptBuf accumulates InputTranscription chunks for the utterance currently being spoken. Session-scoped: a fresh receiveLoop call (every Connect()/reconnect) starts with an empty buffer.
	var clock turnClock
	var inputTranscriptBuf strings.Builder

	// outputTranscriptBuf accumulates OutputTranscription chunks — June's own reply — for the turn currently being spoken, mirroring inputTranscriptBuf. Session-scoped in the same way.
	var outputTranscriptBuf strings.Builder

	// pendingBargeIn is when an Interrupted arrived while June was speaking with nothing yet in the input transcript. Zero means none is waiting. A user transcript inside bargeInConfirmWindow turns it into a real barge-in; anything else lets it expire, which is what the room and the ceiling fan do.
	var pendingBargeIn time.Time

	// recentJuneSpeech holds June's last few completed utterances, for telling the mic hearing her own voice apart from the user talking (see matchesRecentJuneSpeech). Session-scoped in the same way as the buffers above.
	var recentJuneSpeech []juneUtterance

	// turnUsage accumulates the Live API's own token counts for the turn currently in progress, the same way askVoice's eval loop does with TurnTrace.Usage — most server messages of a turn carry no UsageMetadata and addLive is then a no-op, so this only grows on the messages that do. Session-scoped in the same way as the buffers above.
	turnUsage := TokenUsage{Provider: ProviderGemini}

	// flushTurnUsage files turnUsage as the turn that just finished (see VoiceUsage) and resets the accumulator for the next turn, mirroring flushJuneSpeech's reset-after-read pattern for outputTranscriptBuf.
	flushTurnUsage := func() {
		a.recordVoiceTurnUsage(turnUsage)
		turnUsage = TokenUsage{Provider: ProviderGemini}
	}

	// flushJuneSpeech logs the reply June just finished (or was cut off partway through) as one line and resets the buffer. Nothing June said reached the log at all between 7 August and this, which made scoring a session against its own replies impossible: the user's side, the model's thoughts and every tool result were all logged, and the reply itself was not.
	flushJuneSpeech := func() {
		said := strings.TrimSpace(outputTranscriptBuf.String())
		outputTranscriptBuf.Reset()
		if said == "" {
			return
		}
		slog.Info("june said", "text", said)
		// Recorded so a matching mic transcript that comes back within echoWindow can be recognized as June's own voice rather than a new user turn (see matchesRecentJuneSpeech). Trimmed to the last few so a long session doesn't grow this without bound.
		recentJuneSpeech = append(recentJuneSpeech, juneUtterance{text: said, end: time.Now()})
		if len(recentJuneSpeech) > echoHistorySize {
			recentJuneSpeech = recentJuneSpeech[len(recentJuneSpeech)-echoHistorySize:]
		}
	}

	// flushInputTranscript sends whatever's accumulated in inputTranscriptBuf as a SenderYou chunk and resets the buffer — a no-op if nothing's there, so it's safe to call from every "the model is now responding" trigger point without worrying about emitting an empty "you" line on a typed turn. Genai's own Transcription.Finished doc comment says it marks the last chunk, but current Live API model versions never actually set it (documented: googleapis/js-genai#1429 — only fragments arrive, the flag never updates), so this is called from wherever the FIRST sign of a reply shows up, not just Finished: the first OutputTranscription fragment, a ModelTurn message, a ToolCall, TurnComplete/GenerationComplete, or Interrupted. The Finished path is kept too — harmless if a future model version starts firing it.
	// flushInputTranscript's bool return is true only when the buffered text was queued as a genuine user turn — false for empty/non-speech, muted, or an echo of June's own recent speech. The barge-in path below needs this to tell a real interruption from the mic hearing June talk to herself.
	flushInputTranscript := func() bool {
		utterance := strings.TrimSpace(inputTranscriptBuf.String())
		inputTranscriptBuf.Reset()
		if isNonSpeechTranscript(utterance) {
			return false
		}
		// The mic is off (text-only mode, or /mute) — anything the server still transcribes is audio it already had in hand, or the room rather than the user. Dropping it here, at the one choke point every call site goes through, is what stops text-only mode from answering the conversation happening around the machine.
		if a.isMuted.Load() {
			slog.Debug("dropping voice transcript: mic is muted", "text", utterance)
			return false
		}
		// The mic picking up June's own voice and transcribing it as if the user said it: a real production session answered its own greeting, then answered that answer, and looped for minutes. See matchesRecentJuneSpeech.
		if matchesRecentJuneSpeech(recentJuneSpeech, utterance, time.Now()) {
			slog.Debug("ignoring echo of june's own speech", "text", utterance)
			return false
		}
		slog.Info("user said (voice)", "text", utterance)
		select {
		case a.TextResponseChan <- ResponseChunk{Text: utterance, Sender: SenderYou}:
		default:
		}
		return true
	}

	for {
		select {
		case <-recvCtx.Done():
			return
		default:
		}
		msg, err := session.Receive()
		if err != nil {
			recvSpan.RecordError(err)
			recvSpan.SetStatus(codes.Error, "receive failed")
			errChan <- fmt.Errorf("receive loop error: %w", err)
			select {
			case a.ErrorChan <- err:
			default:
			}
			return
		}

		// The Live API reports usage on the server message that finishes a response rather than on a response object, so a turn that runs tools reports usage once per round and they add up here the same way askVoice's eval loop totals TurnTrace.Usage (see TokenUsage.addLive in ask.go). Most messages of a turn carry no UsageMetadata at all, and addLive is then a no-op.
		turnUsage.addLive(msg.UsageMetadata)

		// GoAway: the server is about to hang up (session expiry or rate limits). Logging it and carrying on is what produced "close 1008 client failed to close the connection after receiving a GoAway" three times in 25 minutes on 2026-09-09, each force-close losing whatever a tool delivered in its last seconds. So the session is given up here instead: wait out the sentence June is in the middle of, hang up, and let the caller's reconnect loop redial and resume from the stored handle.
		if msg.GoAway != nil {
			slog.Warn("live session GoAway received, closing the session so the reconnect happens on our terms",
				"time_left", msg.GoAway.TimeLeft)
			a.waitForSpeakerQuiet(goAwayDrainLimit)
			if err := session.Close(); err != nil {
				slog.Warn("closing the live session after a GoAway failed", "error", err)
			}
			errChan <- ErrGoAway
			return
		}

		// SessionResumptionUpdate: capture the handle so the next Connect() can resume instead of cold-starting. Only stored when the server marks the state actually resumable with a non-empty handle — an unresumable point (e.g. mid function-call) sends an empty one, which would otherwise make the next reconnect skip the greeting while still cold-starting.
		if u := msg.SessionResumptionUpdate; u != nil && u.Resumable && u.NewHandle != "" {
			a.setResumeHandle(u.NewHandle)
		}

		// Accumulate InputTranscription chunks. CONFIRMED HARMFUL as of 2026-08-09: auto-injecting a recall via an unsolicited SendClientContent call from here caused a real live session to loop on "is the user still there?" reasoning for 3+ minutes with zero final replies — a stray client turn arriving mid-session breaks native-audio turn-taking. That auto-injection path (injectVoiceRecalls) was deleted in WP6 as dead code — it was never wired back in after this regression — see TestReceiveLoop_InputTranscriptionFinished_DoesNotAutoInject for why any future replacement needs to be re-verified against a real Live session first.
		if msg.ServerContent != nil && msg.ServerContent.InputTranscription != nil {
			it := msg.ServerContent.InputTranscription
			inputTranscriptBuf.WriteString(it.Text)
			if !isNonSpeechTranscript(it.Text) {
				clock.userSpoke(time.Now())
				// The late half of a barge-in: an Interrupted came in while June was talking and nothing had been transcribed yet, and this is that speech arriving. Cut her off now rather than letting her talk over the user for the rest of the sentence.
				if waited := time.Since(pendingBargeIn); !pendingBargeIn.IsZero() && waited < bargeInConfirmWindow {
					pendingBargeIn = time.Time{}
					slog.Info("barge-in confirmed by a late transcript", "waited", waited)
					flushJuneSpeech()
					a.speaker.Flush()
					notice := "[june stopped]"
					if a.typedTurnActive.Load() {
						notice = "[interrupted by voice input — the answer above is cut short]"
					}
					select {
					case a.TextResponseChan <- ResponseChunk{Text: notice, Sender: SenderSystem}:
					default:
					}
				}
			}
			if it.Finished {
				flushInputTranscript()
			}
		}

		// OutputTranscription is the text of June's own spoken audio — with ResponseModalities=[Audio], this is the only complete-text form of what June actually said, since ModelTurn text parts are fragments (see below). Also the first sign the model is responding, so it flushes any pending input-transcript first (see flushInputTranscript's doc comment).
		if msg.ServerContent != nil && msg.ServerContent.OutputTranscription != nil && msg.ServerContent.OutputTranscription.Text != "" {
			flushInputTranscript()
			// Strip control-token artifacts here, before the text reaches either consumer below — the log line and the TUI/voice channel both read from this one spot, so cleaning it once here is enough for both. A chunk that was nothing but artifacts comes back empty and is dropped rather than logged or spoken as if it were real content.
			if text := stripControlTokens(msg.ServerContent.OutputTranscription.Text); text != "" {
				outputTranscriptBuf.WriteString(text)
				select {
				case a.TextResponseChan <- ResponseChunk{Text: text}:
				default:
				}
			}
		}

		// Not every Interrupted flag is a barge-in. Sending a tool result with INTERRUPT scheduling asks the Live server to interrupt its own generation to fold the result in, and the server reports that with the very same flag — measured at 72-80ms after the send on four consecutive tool calls in one production session (2026-08-28, 03:01-03:06). Treating it as a barge-in flushed the audio June was still speaking and wrote "[june stopped]" into the transcript, which is what left the user with a preamble and then silence.
		// The signal that separates them is whether the user is actually saying anything: a real barge-in has InputTranscription accumulating, a tool delivery has none. Checked before flushInputTranscript below, which would empty that buffer. Clearing the flag rather than skipping the message keeps every other handler (ModelTurn audio, TurnComplete) running on it.
		if msg.ServerContent != nil && msg.ServerContent.Interrupted && inputTranscriptBuf.Len() == 0 && a.consumeToolDeliveryInterrupt(time.Now()) {
			slog.Debug("interrupt attributed to tool-result delivery, not a barge-in")
			msg.ServerContent.Interrupted = false
		}

		// check for server-side barge-in (VAD)
		if msg.ServerContent != nil && msg.ServerContent.Interrupted {
			// Checked before flushInputTranscript empties the buffer: an echo of June's own recent speech must not read as a barge-in at all, not just fail to become a user turn — otherwise the flush and speaker.Flush() below still fire on nothing more than the mic hearing her talk to herself.
			if pending := strings.TrimSpace(inputTranscriptBuf.String()); pending != "" && matchesRecentJuneSpeech(recentJuneSpeech, pending, time.Now()) {
				inputTranscriptBuf.Reset()
				slog.Debug("ignoring echo of june's own speech", "text", pending)
				continue
			}
			// Whether the user is actually saying anything is the one signal that separates a real interruption from everything else that trips the server's voice detector.
			userSpoke := flushInputTranscript()
			// Muted means no audio of ours reached the server, so a voice-activity interrupt can only be the room. Ignoring it keeps ambient noise from cutting June off mid-answer in text-only mode.
			if a.isMuted.Load() {
				slog.Debug("ignoring barge-in: mic is muted")
				continue
			}
			// Nobody has said anything yet and June's own speaker is audibly running. This is either the room (a ceiling fan produced 47 of these in 17 minutes, every one flushing the audio mid-sentence) or a real interruption whose transcript has not landed yet — the server raises Interrupted on voice detection, and the words follow a few hundred milliseconds later. Hold it open instead of deciding now: the InputTranscription branch above cuts June off if speech arrives inside bargeInConfirmWindow, and nothing happens if it does not.
			if !userSpoke && a.speaker.CurrentAmplitude() > bargeInEchoAmplitude {
				pendingBargeIn = time.Now()
				slog.Debug("barge-in held open: no user transcript yet while june is speaking")
				continue
			}
			pendingBargeIn = time.Time{}
			slog.Info("barge-in detected: server interrupted model generation", "user_spoke", userSpoke)
			flushJuneSpeech()
			a.speaker.Flush()

			// The notice explains a cut-off answer, so it is only honest when the user actually cut in. It was written 62 times in one day with no user transcript behind almost any of them, each one telling the user they interrupted a reply they never touched.
			if !userSpoke {
				continue
			}
			// The generation is already cancelled server-side and cannot be resumed, so the answer just stops mid-sentence. Whatever streamed stays in the transcript; this line says why it ends where it does. A typed turn gets the explicit wording — "you interrupted June" is the wrong story to tell someone who typed the question and never spoke.
			notice := "[june stopped]"
			if a.typedTurnActive.Load() {
				notice = "[interrupted by voice input — the answer above is cut short]"
			}
			select {
			case a.TextResponseChan <- ResponseChunk{Text: notice, Sender: SenderSystem}:
			default:
			}
			continue
		}

		// this has very interesting spanning logic
		if msg.ServerContent != nil && msg.ServerContent.ModelTurn != nil {
			a.ensureTurnID()
			flushInputTranscript()
			_, turnSpan := otelTracer.Start(recvCtx, "Agent.ModelTurn")

			var audioBytes int
			var textContent string

			for _, part := range msg.ServerContent.ModelTurn.Parts {
				if part.Text != "" {
					if part.Thought {
						slog.Debug("june thought", "text", part.Text)
						select {
						case a.TextResponseChan <- ResponseChunk{Text: part.Text, IsThought: true}:
						default:
						}
					} else {
						// Not forwarded to TextResponseChan: under ResponseModalities=[Audio], non-thought ModelTurn text parts are incomplete fragments — OutputTranscription (handled above) is the complete-text source.
						slog.Debug("june response fragment (not forwarded, see OutputTranscription)", "text", part.Text)
						textContent = part.Text
					}
				}
				// if part is audio
				if part.InlineData != nil {
					if d, ok := clock.firstSound(time.Now()); ok {
						slog.Info("first sound", "after_user_ms", d.Milliseconds())
					}
					audioBytes += len(part.InlineData.Data)
					a.speaker.Play(part.InlineData.Data)
				}
			}

			// always set span attributes, even for audio-only turns
			turnSpan.SetAttributes(
				attribute.String("llm.model_name", model),
				attribute.Int("llm.audio_bytes", audioBytes),
			)
			if textContent != "" {
				turnSpan.SetAttributes(attribute.String("llm.output_messages", textContent))
			}
			turnSpan.End()
		}

		// TurnComplete/GenerationComplete mark the end of one model turn — the boundary the UI needs so it stops merging this turn's june chunks into whatever arrives for the NEXT turn (see streamLine's TurnBoundary handling). The SDK can signal either depending on realtime-playback timing, so both are checked; Interrupted (handled above) already breaks the merge chain on its own since it emits a system-sender chunk.
		if msg.ServerContent != nil && (msg.ServerContent.TurnComplete || msg.ServerContent.GenerationComplete) {
			a.endTurn()
			clock.turnDone()
			flushInputTranscript()
			flushJuneSpeech()
			flushTurnUsage()
			// The typed turn (if this was one) is over — a later interruption belongs to whatever comes next. A barge-in still waiting for its transcript goes with it: the turn it would have cut off has already ended on its own.
			a.typedTurnActive.Store(false)
			pendingBargeIn = time.Time{}
			select {
			case a.TextResponseChan <- ResponseChunk{TurnBoundary: true}:
			default:
			}
		}

		// tool calling - check if model wants to use a tool.
		//
		// Dispatched to its own goroutine per call: executeTool can block for an arbitrary time (HITL shell_exec waits on TUI approval), and running it inline here used to stall session.Receive() for the duration, backing up the receive buffer and dropping mic frames in audioSendLoop downstream. Now Receive() keeps getting called immediately; the tool's result is sent back later, matched by fc.ID/fc.Name.
		// One goroutine per MESSAGE, not per call: the calls of one model turn are steps of one plan and run in the order the model gave them. Measured 2026-09-09: click(tab) and press_key Ctrl+W arrived together, the key landed after 62 ms and the click after 666 ms, so the wrong tab closed; click(search box) and type_text arrived together three times and the typing finished before the click landed, so the box stayed empty. Receive() is still never blocked, which is what the goroutine is for.
		if msg.ToolCall != nil {
			flushInputTranscript()
			calls := msg.ToolCall.FunctionCalls
			for _, fc := range calls {
				slog.Info("tool call received", "tool", fc.Name, "args", fc.Args)
			}
			toolCtx := WithTurnID(recvCtx, a.currentTurnID())
			go func() {
				for _, fc := range calls {
					a.runToolCall(toolCtx, otelTracer, session, fc)
				}
			}()
		}
	}
}

// quietTools are the tools whose result the user is not sitting there waiting to hear.
// Saving, correcting or forgetting a note, and opening a URL, all produce their real effect outside the conversation — the user sees the browser open, or simply trusts that the note was saved — so their result is scheduled WHEN_IDLE and slots into the next natural gap instead of cutting off whatever June is saying.
var quietTools = map[string]bool{
	"save_note": true,
	"add_task":  true,
	"revise":    true,
	"open_url":  true,
}

// scheduleFor picks when a NON_BLOCKING tool's result is folded back into the conversation.
// A quiet or unrecognized tool always waits for an idle moment: quietTools produce their real effect outside the conversation, and an unrecognized name is by definition not one the model was told to announce.
// Every other tool answers a question the user just asked out loud, so it wants INTERRUPT — but only when June has gone quiet. Sending INTERRUPT while she is still speaking would ask the Live server to cancel her current generation to fold the result in, which cuts her own sentence off mid-word; WHEN_IDLE there lets the sentence finish and folds the result in right after.
// Input: the tool's name and whether June is audibly speaking right now. Output: the scheduling to send with that tool's FunctionResponse.
func scheduleFor(name string, speaking bool) genai.FunctionResponseScheduling {
	if _, known := knownToolNames[name]; !known {
		return genai.FunctionResponseSchedulingWhenIdle
	}
	if quietTools[name] {
		return genai.FunctionResponseSchedulingWhenIdle
	}
	if speaking {
		return genai.FunctionResponseSchedulingWhenIdle
	}
	return genai.FunctionResponseSchedulingInterrupt
}

// isSpeaking reports whether June's own voice is audible right now, the same check receiveLoop's echo guard uses.
// Input: none, reads a.speaker. Output: false when there is no speaker (e.g. in a test harness), otherwise whether the current amplitude is above bargeInEchoAmplitude.
func (a *Agent) isSpeaking() bool {
	return a.speaker != nil && a.speaker.CurrentAmplitude() > bargeInEchoAmplitude
}

// toolInterruptWindow is how long after an INTERRUPT-scheduled FunctionResponse send an Interrupted event is credited to that delivery instead of to the user. Production measured 72-80ms; a second is more than ten times that and still far shorter than a person deciding to cut in.
const toolInterruptWindow = time.Second

// longRunNudgeDelay is how long a tool may run before June tells the user it's still on it. A var, not a const, only so tests can shrink it.
// Sent as an interim FunctionResponse with WillContinue set — the generator form of a NON_BLOCKING call, and the only turn-safe way to inject anything into a tool exchange. A bare out-of-turn SendClientContent is not: one broke native-audio turn-taking for three minutes in a real session (see the InputTranscription comment in receiveLoop).
// Five seconds, down from eight: at eight a 30-second open_app left two silences of nearly the length of a held breath before June said anything, and the user heard the tool as a hang rather than as work in progress. The gaps still double from here (see nudgeGap), so a genuinely long job does not become chatter.
var longRunNudgeDelay = 5 * time.Second

// knownToolNames is the set of names in toolDefinitions, built once so scheduleFor can tell an unrecognized tool from a declared one.
var knownToolNames = func() map[string]struct{} {
	names := map[string]struct{}{}
	for _, tool := range toolDefinitions() {
		for _, fd := range tool.FunctionDeclarations {
			names[fd.Name] = struct{}{}
		}
	}
	return names
}()

// runToolCall executes a single function call and sends its result back to the model.
// Meant to run in its own goroutine (via receiveLoop) so a slow/blocking tool never stalls session.Receive(). A panic anywhere in the call is recovered, logged with its stack, reported to the model as a failed tool call, and closed out in the UI's activity feed, so one bad tool never takes the daemon down or leaves the transcript showing a call that is still "running".
func (a *Agent) runToolCall(ctx context.Context, tracer trace.Tracer, session liveSession, fc *genai.FunctionCall) {
	// This runs on a bare goroutine spawned from receiveLoop, with nothing above it to catch a panic — a bad type assertion or a nil dereference in a tool handler would otherwise take the whole daemon down. Caught here, it is logged, reported to the model as a failed tool call rather than silence, and the UI's "running" entry for it is closed out instead of left stuck.
	defer func() {
		if r := recover(); r != nil {
			slog.Error("tool call panicked", "tool", fc.Name, "panic", r, "stack", string(debug.Stack()))
			a.sendToolActivity(ToolActivity{
				ID:    fc.ID,
				Name:  fc.Name,
				Phase: ToolFinished,
				Err:   true,
			})
			if ctx.Err() != nil {
				return
			}
			a.writeMu.Lock()
			if err := session.SendToolResponse(genai.LiveSendToolResponseParameters{
				FunctionResponses: []*genai.FunctionResponse{{
					ID:       fc.ID,
					Name:     fc.Name,
					Response: map[string]any{"output": "error: an internal failure interrupted this tool call; try again or take another route"},
				}},
			}); err != nil {
				slog.Error("failed to send the panic tool response", "error", err)
			}
			a.writeMu.Unlock()
		}
	}()

	_, toolSpan := tracer.Start(ctx, "Agent.ToolExecution")
	defer toolSpan.End()
	toolSpan.SetAttributes(attribute.String("tool.name", fc.Name))

	started := time.Now()
	// Computed once, sent on both events: Finished needs its own ArgsSummary too, so the UI's transcript stays correct even if a second concurrent call has already taken over the "live" status by the time this one finishes.
	argsSummary := toolActivitySummary(fc.Name, fc.Args)
	a.sendToolActivity(ToolActivity{
		ID:          fc.ID,
		Name:        fc.Name,
		ArgsSummary: argsSummary,
		Phase:       ToolStarted,
		Started:     started,
	})

	scheduling := scheduleFor(fc.Name, a.isSpeaking())
	toolStart := time.Now()
	result := a.runToolWithNudge(ctx, session, fc, scheduling)

	// Safety: truncate massive results to prevent a 1011 crash.
	if len(result) > 10000 {
		result = util.UTF8Bytes(result, 10000) + "\n\n[Output Truncated: Result too large for Live context]"
		slog.Warn("tool result truncated", "tool", fc.Name, "length", len(result))
	}

	// Kept for a short while so a reconnect can tell the resumed session what came back, whether or not this send reaches the model: the socket can die between the send and the server folding the result in, and on 2026-09-09 one did, nine seconds after a branch answered. See branchNoteWindow.
	if fc.Name == "branch" {
		task, _ := fc.Args["task"].(string)
		a.rememberBranchResult(task, result, time.Now())
	}

	slog.Info("tool result", "tool", fc.Name, "took_ms", time.Since(toolStart).Milliseconds(), "result", result)

	// Emitted before the ctx.Err() check below on purpose: ToolActivityChan is Agent-scoped (survives reconnects, like TextResponseChan), so the UI's transcript stays accurate even after the session that spawned this call is gone.
	a.sendToolActivity(ToolActivity{
		ID:            fc.ID,
		Name:          fc.Name,
		ArgsSummary:   argsSummary,
		Phase:         ToolFinished,
		Started:       started,
		ResultSummary: resultSummary(fc.Name, result),
		Err:           strings.HasPrefix(result, "error"),
	})

	// The session that spawned this call is gone (e.g. a HITL approval that never arrived before disconnect) — nothing is listening, so skip the write instead of contending writeMu on a session being torn down.
	if ctx.Err() != nil {
		// branch is the one tool worth recovering: it can run tens of seconds across several model round trips, so "session died mid-call" is a real case, not the millisecond non-issue every other tool call is. Persist it so it surfaces at the next handshake instead of vanishing like everything else's dropped response.
		if fc.Name == "branch" && !strings.HasPrefix(result, "error") {
			task, _ := fc.Args["task"].(string)
			if _, err := a.brain.SaveFold(context.Background(), task, result); err != nil {
				slog.Error("branch: fallback fold persistence also failed — result is lost", "error", err)
			}
		}
		slog.Warn("dropping tool response: session ended before it could be delivered", "tool", fc.Name)
		return
	}

	// Recomputed here, not reused from before the tool ran: the tool may have taken seconds, and whether June is speaking now — not when the call started — is what decides whether an INTERRUPT would cut her off.
	scheduling = scheduleFor(fc.Name, a.isSpeaking())

	// Marked before the send, not after: the server's fold-in interrupt comes back within ~80ms, and receiveLoop must already see the record by then. See consumeToolDeliveryInterrupt.
	if scheduling == genai.FunctionResponseSchedulingInterrupt {
		a.markToolResponseSent(time.Now())
	}

	// send result back to model with matching ID
	a.writeMu.Lock()
	err := session.SendToolResponse(genai.LiveSendToolResponseParameters{
		FunctionResponses: []*genai.FunctionResponse{{
			ID:   fc.ID,
			Name: fc.Name,
			// Scheduling only has an effect because the declarations are NON_BLOCKING (see toolDefinitions); on a BLOCKING call the API ignores it.
			Scheduling: scheduling,
			Response:   map[string]any{"output": result},
		}},
	})
	a.writeMu.Unlock()
	if err != nil {
		toolSpan.RecordError(err)
		slog.Error("failed to send tool response", "error", err)
	}
}

// nudgeLines are what a mid-tool nudge tells the model, in order, one per longRunNudgeDelay the tool overruns by. The model speaks its own words from these, so they are states rather than lines to read out: the point is that the second nudge does not say what the first said. A single repeated sentence reads as a stuck loop, which is worse than silence.
//
// They also carry roughly how long it has been, because the model has no clock of its own mid-call and "a while now" is the difference between "still going" and "this has hung". The last line stands for every nudge after it: past a minute there is nothing new to say, and the honest thing is to keep saying it rather than escalate to a promise the tool may not keep.
var nudgeLines = []string{
	"still running, no result yet",
	"still running, a good few seconds in now — say so, differently from last time",
	"still running, about half a minute in — worth telling them it is taking longer than usual",
	"still running, over a minute in — tell them plainly that it is slow and let them decide whether to wait",
}

// nudgeLine is what the nth nudge for one tool call says, counting from 1. Input: which nudge this is. Output: that line, or the last one for every nudge past the end of nudgeLines.
func nudgeLine(n int) string {
	if n < 1 {
		n = 1
	}
	if n > len(nudgeLines) {
		n = len(nudgeLines)
	}
	return nudgeLines[n-1]
}

// nudgeBackoffCap is the widest gap between two nudges. Half a minute: past that a person waiting has stopped expecting a running commentary, and a check-in every thirty seconds still says the thing has not been forgotten.
const nudgeBackoffCap = 30 * time.Second

// nudgeGap is how long to wait before the nth nudge, counting from 1. Input: which nudge this is. Output: longRunNudgeDelay for the first, then double the gap each time, held at nudgeBackoffCap.
//
// The gaps widen because a fixed interval turns into nagging. At a flat eight seconds a tool stuck for five minutes speaks thirty-seven times, which reads as a loop rather than as patience. Doubling gives eight seconds, then sixteen, then thirty, and about eleven check-ins over those five minutes — which is what someone half-listening actually wants: quick reassurance early, while it still might finish, then the occasional reminder that it has not been dropped.
func nudgeGap(n int) time.Duration {
	gap := longRunNudgeDelay
	for i := 1; i < n; i++ {
		gap *= 2
		if gap >= nudgeBackoffCap {
			return nudgeBackoffCap
		}
	}
	if gap > nudgeBackoffCap {
		return nudgeBackoffCap
	}
	return gap
}

// runToolWithNudge runs the tool and, for as long as it keeps running, sends an interim FunctionResponse every longRunNudgeDelay telling the model the call hasn't finished — so it can keep saying "still on it" out loud instead of leaving the user in silence through a long operation.
//
// It nudges repeatedly, not once. One nudge and then silence is what made the 30-second open_app on 2026-09-12 feel like being dropped: June said "that one's still going", then said nothing for the remaining 22 seconds while the user asked "can you hear me?". Each nudge carries a different line (see nudgeLines) so the model has something new to say each time.
// Only INTERRUPT-scheduled tools get a nudge: a WHEN_IDLE result (saving a note, opening a URL) is not something the user is waiting through silence for.
// Input: the tool call and the scheduling its result will carry. Output: the tool's result string, exactly as executeTool returned it.
func (a *Agent) runToolWithNudge(ctx context.Context, session liveSession, fc *genai.FunctionCall, scheduling genai.FunctionResponseScheduling) string {
	// The tool gets a road for a picture here, where the session is in hand: look pushes its capture down it rather than trying to return it, which a function response cannot carry.
	ctx = WithPictureSender(ctx, a.sessionPictureSender(session))
	if scheduling != genai.FunctionResponseSchedulingInterrupt {
		return a.executeTool(ctx, fc.Name, fc.Args)
	}

	done := make(chan string, 1)
	go func() { done <- a.executeTool(ctx, fc.Name, fc.Args) }()

	sent := 0
	timer := time.NewTimer(nudgeGap(1))
	defer timer.Stop()
	for {
		select {
		case result := <-done:
			return result
		case <-ctx.Done():
			// The session is going away. Wait for the tool rather than leaking the goroutine holding it; the caller is tearing down anyway and nothing will read another nudge.
			return <-done
		case <-timer.C:
		}
		sent++
		timer.Reset(nudgeGap(sent + 1))
		line := nudgeLine(sent)
		// Recomputed at each nudge, not reused from the caller: time has passed, so whether June is speaking now is what decides whether INTERRUPT would cut her off.
		nudgeScheduling := scheduleFor(fc.Name, a.isSpeaking())
		slog.Info("tool still running, sending a progress nudge", "tool", fc.Name, "nudge", sent, "after", time.Duration(sent)*longRunNudgeDelay)
		if nudgeScheduling == genai.FunctionResponseSchedulingInterrupt {
			a.markToolResponseSent(time.Now())
		}
		a.writeMu.Lock()
		err := session.SendToolResponse(genai.LiveSendToolResponseParameters{
			FunctionResponses: []*genai.FunctionResponse{{
				ID:           fc.ID,
				Name:         fc.Name,
				Scheduling:   nudgeScheduling,
				WillContinue: genai.Ptr(true),
				Response:     map[string]any{"output": line},
			}},
		})
		a.writeMu.Unlock()
		if err != nil {
			// A send that failed is a session that has most likely gone. Stop nudging into it and just wait for the tool, rather than failing the same write every longRunNudgeDelay until it finishes.
			slog.Warn("failed to send the mid-tool progress nudge, not sending more", "tool", fc.Name, "nudge", sent, "error", err)
			return <-done
		}
	}
}

// sendToolActivity is a non-blocking send, same drop-on-full pattern as every other Agent channel (TextResponseChan, ErrorChan) — a UI that isn't draining ToolActivityChan must never be able to stall a real tool call.
func (a *Agent) sendToolActivity(ev ToolActivity) {
	select {
	case a.ToolActivityChan <- ev:
	default:
	}
}

// micInput wraps one chunk of 24 kHz mono PCM as realtime audio input. It uses the Audio field, which serialises to realtime_input.audio; the Media field serialises to media_chunks, which the Live API deprecated with the gemini-3 models.
// mutedKeepalive is how often the mic loop sends a chunk of silence while the user is muted. The gemini-3 Live models close a session that has heard no audio from the client for about 150 seconds (measured 2026-09-03: 2m32s across three silent probes, while the 2.5 model stayed open past ten minutes), and an afternoon on mute produced nine drops. A chunk every 60 seconds did not keep it open (closed at 2m51s); a chunk every 10 seconds or every second did (still open at 7 minutes), and a text turn only bought another 150 seconds. Ten seconds is the slowest rate shown to work, at a few audio tokens a minute.
const mutedKeepalive = 10 * time.Second

// mutedInput decides what the mic loop sends while the user is muted. Input: the length of the mic chunk being dropped and the time since anything was last sent. Output: a chunk of silence of the same length and true once every mutedKeepalive, or nil and false in between.
func mutedInput(n int, sinceLast time.Duration) ([]byte, bool) {
	if sinceLast < mutedKeepalive {
		return nil, false
	}
	return make([]byte, n), true
}

func micInput(pcm []byte) genai.LiveRealtimeInput {
	return genai.LiveRealtimeInput{Audio: &genai.Blob{Data: pcm, MIMEType: "audio/pcm;rate=24000"}}
}

func (a *Agent) audioSendLoop(ctx context.Context, session *genai.Session, micChan <-chan []byte, errChan chan error) {
	otelTracer := obs.GetTracer(ctx, "june.agent")
	sendCtx, sendSpan := otelTracer.Start(ctx, "Agent.SendLoop")
	defer sendSpan.End()
	lastSent := time.Now()
	for {
		select {
		case pcm := <-micChan:
			if a.isMuted.Load() {
				chunk, send := mutedInput(len(pcm), time.Since(lastSent))
				if !send {
					continue
				}
				pcm = chunk
			}
			input := micInput(pcm)
			lastSent = time.Now()

			a.writeMu.Lock()
			err := session.SendRealtimeInput(input)
			a.writeMu.Unlock()

			if err != nil {
				sendSpan.RecordError(err)
				sendSpan.SetStatus(codes.Error, "send failed")
				errChan <- fmt.Errorf("failed to send audio: %w", err)
				select {
				case a.ErrorChan <- err:
				default:
				}
				return
			}
		case <-sendCtx.Done():
			return
		}
	}
}

// textSendLoopRetrieveTimeout bounds how long a typed turn waits on RetrieveRelevant before sending anyway — RetrieveRelevant's embed call (now a local llama-server request over daemon IPC) has no deadline of its own, so a slow/hung call would otherwise delay delivering the user's message to the live session by however long that takes. On expiry RetrieveRelevant's own ctx.Err() just means recalls comes back empty; HybridSearch already degrades the same way when its embed call fails.
// This is a ceiling, not a wait: the call returns the moment retrieval finishes and cancel() runs on the next line, so a fast retrieval costs nothing. Nothing on the path sleeps — HybridSearch runs the embed and the index query and returns. Checked against the 2026-08-28 log: zero "retrieve relevant timed out or failed" lines across 170 typed turns, and memory tool calls that day ran a 12ms median (63ms for the slowest query_memory), so the budget is never actually spent. The only case that can consume it is a cold llama-server start, which happens once. Left at three seconds for that case rather than tuned down for a cost that is not being paid.
const textSendLoopRetrieveTimeout = 3 * time.Second

func (a *Agent) textSendLoop(ctx context.Context, session liveSession) {
	for {
		select {
		case text := <-a.TextChan:
			if text == "" {
				continue
			}
			// flush when barge-in and stop talking immediately
			// this is voice haha
			a.speaker.Flush()
			slog.Debug("sending text to model", "text", text)
			a.markTypedTurn()

			recallCtx, cancel := context.WithTimeout(ctx, textSendLoopRetrieveTimeout)
			recalls, err := a.brain.RetrieveRelevant(recallCtx, text, 2)
			cancel()
			if err != nil {
				slog.Warn("retrieve relevant timed out or failed, sending without recalls", "error", err)
			}
			a.writeMu.Lock()
			// re-inject "now" (+ any recalls) every turn since the system prompt is frozen at handshake — otherwise a long conversation drifts off the current date. Sent as a second Part in the same turn as the user's text, not a separate SendClientContent call (see buildTurnContent).
			sendErr := session.SendClientContent(genai.LiveSendClientContentParameters{
				Turns: buildTurnContent(time.Now(), recalls, text),
			})
			a.writeMu.Unlock()

			if sendErr != nil {
				slog.Error("failed to send text", "error", sendErr)
			}
		case <-ctx.Done():
			return
		}
	}
}
