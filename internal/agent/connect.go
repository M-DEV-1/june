package agent

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"ora/internal/config"
	"ora/internal/obs"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/genai"
)

// truncateUTF8 returns the longest prefix of s that fits within maxBytes without splitting a multi-byte UTF-8 rune in half.
// A raw byte-index slice (s[:maxBytes]) can land mid-rune — e.g. cutting into one of a11y capture's 3-byte U+FFFC chars — leaving a broken trailing byte sequence sent to the Live API.
func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	i := maxBytes
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i]
}

// nowAnchor renders the current moment for the system prompt — weekday, date, wall-clock time, timezone — so the model can resolve "yesterday", "this morning", or "July 5th" into concrete dates instead of guessing.
// The recall tool's since/until args depend on the model knowing this.
func nowAnchor(now time.Time) string {
	return now.Format("Monday, 2 January 2006, 15:04 MST")
}

// re-sends the time every turn since the system prompt is frozen at handshake — otherwise the date goes stale mid-conversation. Relevant memory rides along in the same payload.
func turnContext(now time.Time, recalls []string) string {
	b := "[context] It is now " + nowAnchor(now) + "."
	if len(recalls) > 0 {
		b += " Relevant memory: " + strings.Join(recalls, " ")
	}
	return b
}

// buildTurnContent packs the per-turn context and the user's real text into one Content/turn as two Parts, not two separate SendClientContent calls.
// TurnComplete defaults to true when unset, so two calls were two complete turns — letting the model reply to the bare context line and doubling Live API round-trips per utterance.
func buildTurnContent(now time.Time, recalls []string, text string) []*genai.Content {
	return []*genai.Content{
		{
			Role: "user",
			Parts: []*genai.Part{
				{Text: turnContext(now, recalls)},
				{Text: text},
			},
		},
	}
}

func (a *Agent) Connect(ctx context.Context, micChan <-chan []byte) error {
	// branchCalls is scoped to one live session — reset at the top of every Connect() (fresh or reconnect) so a prior session hitting maxBranchesPerSession doesn't leave branch() dead for the rest of the process.
	a.branchCalls.Store(0)

	tracer := obs.GetTracer(ctx, "ora.agent")
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
	span.SetAttributes(attribute.String("agent.model", config.VoiceModel))

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

	// current uncompiled buffer (immediate ctx); derive a simple focus string from [working] items (app+title keywords) and use it as a signal for relevance via SearchMemory when the compiler buffer is available.
	if a.compiler != nil {
		buffer := a.compiler.GetCurrentBuffer()
		var focusParts []string
		for _, act := range buffer {
			contextParts = append(contextParts, fmt.Sprintf("  [working] %s: %s", act.App, act.Title))
			focusParts = append(focusParts, act.App, act.Title)
		}
		if focus := strings.Join(focusParts, " "); focus != "" {
			if hits, err := a.brain.SearchMemory(handshakeCtx, focus); err == nil {
				for i, h := range hits {
					if i >= 2 {
						break
					}
					if h.Source == "note" {
						contextParts = append(contextParts, "  [note] "+h.Content)
					} else {
						contextParts = append(contextParts, fmt.Sprintf("  [%s] %s", h.Source, h.Content))
					}
				}
			}
		}
	}

	contextStr := strings.Join(contextParts, "\n")

	tools := liveTools()
	toolsCount := 0
	if len(tools) > 0 {
		toolsCount = len(tools[0].FunctionDeclarations)
	}

	// config
	// Voice is configurable via /voice in the TUI (see config.AvailableVoices); falls back to config.DefaultVoice if unset or invalid.
	voiceName := a.GetVoice()
	if voiceName == "" || !config.IsValidVoice(voiceName) {
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
		SessionResumption: &genai.SessionResumptionConfig{Handle: priorHandle},
		Tools:             tools,
		// Without this, a session never trims its own context and eventually exhausts it (root cause of "voice breaks in longer conversations"). Trigger/target follow the recommended Gemini Live defaults: compress near ~25k tokens, shrink back to ~8k.
		ContextWindowCompression: &genai.ContextWindowCompressionConfig{
			TriggerTokens: genai.Ptr[int64](25000),
			SlidingWindow: &genai.SlidingWindow{TargetTokens: genai.Ptr[int64](8000)},
		},
		SystemInstruction: &genai.Content{
			Role: "system",
			Parts: []*genai.Part{
				{
					Text: fmt.Sprintf("You are Ora. You've been alongside the user through their day — you notice what they're working on, you remember what came before, and you carry it so they never have to re-explain themselves. You're easy to talk to and genuinely invested in how things are going for them, and you're also the one who quietly gets things done when asked.\n\nVoice and manner:\n- Everything you say is spoken out loud. NEVER open with a meta-acknowledgement — no 'acknowledged', 'understood', 'got it', 'sure', 'okay', 'noted' — and never narrate what you're about to do. There is no instruction to confirm; just say the actual thing, the way a person would.\n- Talk like someone who knows them, not an assistant reading a status report. Speak WITH them, never ABOUT them — no 'here is what this person did.'\n- Keep it short and natural; this is real-time voice. No markdown, no bullet lists, no rattling off long enumerations.\n- Surface what you remember the way a person would — woven in, in passing — not recited back.\n\nThe context below is the user's own record of their own day, on their own machine, kept for them. It's there so you can actually be useful. If they ask what they're doing, watching, working on, or did earlier, just answer — that's the whole point of it. Never fall back on privacy to dodge a question about their own day; refusing to remember it would be a strange thing for you to do. If something genuinely isn't in there, say so and offer to dig — don't guess. And when something stands out, it's fine to ask after it naturally ('how'd that meeting end up going?').\n\nMemory is not append-only — you can fix it. If the user says something you saved was misheard, wrong, or should be forgotten, don't just apologize and move on: look it up with query_memory (its results show notes as \"[note#N] ...\"), then call update_note with the corrected fact or delete_note to remove it, right there in the same conversation. Leaving a known-wrong fact sitting in memory is a bug, not a harmless slip.\n\nRight now it is %s — use this as your anchor for anything time-related (\"yesterday\", \"this morning\", \"earlier today\"); when you recall a timeline, convert the period they mean into concrete since/until dates yourself.\n\nSystem: %s / %s, shell %s.\n\nWhere things stand with them right now, from memory:\n%s\n\nYou have %d tools, plus real-time web search. Use shell_exec to run things when asked, with the right shell for the OS (powershell on windows, sh on linux/mac). Check before anything destructive. For anything outside their own life — current events, facts, prices, anything you're not sure of — search instead of guessing; never state something as fact from memory alone when you could just look it up.", nowAnchor(time.Now()), runtime.GOOS, runtime.GOARCH, shellName(), contextStr, toolsCount),
				},
			},
		},
	}

	slog.Debug("connecting to live API")

	_, wsSpan := tracer.Start(handshakeCtx, "Agent.LiveConnectWebSocket")
	session, err := client.Live.Connect(handshakeCtx, config.VoiceModel, cfg)
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

// liveSession is the subset of *genai.Session's behavior receiveLoop depends on.
// Extracting it as an interface lets tests exercise receiveLoop's concurrency behavior (notably: a slow tool call must not block the receive path) against a fake session instead of a live websocket.
// *genai.Session satisfies this structurally already — no wrapping needed at call sites.
type liveSession interface {
	Receive() (*genai.LiveServerMessage, error)
	SendToolResponse(genai.LiveSendToolResponseParameters) error
}

func (a *Agent) receiveLoop(ctx context.Context, session liveSession, model string, errChan chan error) {
	otelTracer := obs.GetTracer(ctx, "ora.agent")
	recvCtx, recvSpan := otelTracer.Start(ctx, "Agent.ReceiveLoop")
	defer recvSpan.End()

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

		// GoAway: the server is about to hang up (session expiry or rate limits) — otherwise receiveLoop just silently hits a raw close. Just make it observable; the caller's reconnect loop already redials, and can now resume via SessionResumption instead of cold-starting.
		if msg.GoAway != nil {
			slog.Warn("live session GoAway received, server will disconnect soon",
				"time_left", msg.GoAway.TimeLeft)
		}

		// SessionResumptionUpdate: capture the handle so the next Connect() can resume instead of cold-starting. Only stored when the server marks the state actually resumable with a non-empty handle — an unresumable point (e.g. mid function-call) sends an empty one, which would otherwise make the next reconnect skip the greeting while still cold-starting.
		if u := msg.SessionResumptionUpdate; u != nil && u.Resumable && u.NewHandle != "" {
			a.setResumeHandle(u.NewHandle)
		}

		// check for server-side barge-in (VAD)
		if msg.ServerContent != nil && msg.ServerContent.Interrupted {
			slog.Info("barge-in detected: server interrupted model generation")
			a.speaker.Flush()

			// optional: send a visual cue to the UI
			// TODO: remove in dev, or idk keep it
			select {
			case a.TextResponseChan <- "\n*[ora stopped]*\n":
			default:
			}
			continue
		}

		// this has very interesting spanning logic
		if msg.ServerContent != nil && msg.ServerContent.ModelTurn != nil {
			_, turnSpan := otelTracer.Start(recvCtx, "Agent.ModelTurn")

			var audioBytes int
			var textContent string

			for _, part := range msg.ServerContent.ModelTurn.Parts {
				if part.Text != "" {
					slog.Info("ora response", "text", part.Text)
					textContent = part.Text
					select {
					case a.TextResponseChan <- part.Text:
					default:
					}
				}
				// if part is audio
				if part.InlineData != nil {
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

		// tool calling - check if model wants to use a tool.
		//
		// Dispatched to its own goroutine per call: executeTool can block for an arbitrary time (HITL shell_exec waits on TUI approval), and running it inline here used to stall session.Receive() for the duration, backing up the receive buffer and dropping mic frames in audioSendLoop downstream. Now Receive() keeps getting called immediately; the tool's result is sent back later, matched by fc.ID/fc.Name.
		if msg.ToolCall != nil {
			for _, fc := range msg.ToolCall.FunctionCalls {
				slog.Info("tool call received", "tool", fc.Name, "args", fc.Args)
				go a.runToolCall(recvCtx, otelTracer, session, fc)
			}
		}
	}
}

// runToolCall executes a single function call and sends its result back to the model.
// Meant to run in its own goroutine (via receiveLoop) so a slow/blocking tool never stalls session.Receive().
func (a *Agent) runToolCall(ctx context.Context, tracer trace.Tracer, session liveSession, fc *genai.FunctionCall) {
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

	result := a.executeTool(ctx, fc.Name, fc.Args)

	// Safety: truncate massive results to prevent a 1011 crash.
	if len(result) > 10000 {
		result = truncateUTF8(result, 10000) + "\n\n[Output Truncated: Result too large for Live context]"
		slog.Warn("tool result truncated", "tool", fc.Name, "length", len(result))
	}

	slog.Info("tool result", "tool", fc.Name, "result", result)

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

	// send result back to model with matching ID
	a.writeMu.Lock()
	err := session.SendToolResponse(genai.LiveSendToolResponseParameters{
		FunctionResponses: []*genai.FunctionResponse{{
			ID:       fc.ID,
			Name:     fc.Name,
			Response: map[string]any{"output": result},
		}},
	})
	a.writeMu.Unlock()
	if err != nil {
		toolSpan.RecordError(err)
		slog.Error("failed to send tool response", "error", err)
	}
}

// sendToolActivity is a non-blocking send, same drop-on-full pattern as every other Agent channel (TextResponseChan, ErrorChan) — a UI that isn't draining ToolActivityChan must never be able to stall a real tool call.
func (a *Agent) sendToolActivity(ev ToolActivity) {
	select {
	case a.ToolActivityChan <- ev:
	default:
	}
}

func (a *Agent) audioSendLoop(ctx context.Context, session *genai.Session, micChan <-chan []byte, errChan chan error) {
	otelTracer := obs.GetTracer(ctx, "ora.agent")
	sendCtx, sendSpan := otelTracer.Start(ctx, "Agent.SendLoop")
	defer sendSpan.End()
	for {
		select {
		case pcm := <-micChan:
			if a.isMuted.Load() {
				continue // drop mic input if muted
			}
			input := genai.LiveRealtimeInput{
				Media: &genai.Blob{
					Data:     pcm,
					MIMEType: "audio/pcm;rate=24000",
				},
			}

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

func (a *Agent) textSendLoop(ctx context.Context, session *genai.Session) {
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

			recalls, _ := a.brain.RetrieveRelevant(ctx, text, 2)
			a.writeMu.Lock()
			// re-inject "now" (+ any recalls) every turn since the system prompt is frozen at handshake — otherwise a long conversation drifts off the current date. Sent as a second Part in the same turn as the user's text, not a separate SendClientContent call (see buildTurnContent).
			err := session.SendClientContent(genai.LiveSendClientContentParameters{
				Turns: buildTurnContent(time.Now(), recalls, text),
			})
			a.writeMu.Unlock()

			if err != nil {
				slog.Error("failed to send text", "error", err)
			}
		case <-ctx.Done():
			return
		}
	}
}
