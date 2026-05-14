package agent

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"strings"

	"ora/internal/obs"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"google.golang.org/genai"
)

func (a *Agent) Connect(ctx context.Context) error {
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
	model := "models/gemini-2.5-flash-native-audio-latest"
	span.SetAttributes(attribute.String("agent.model", model))

	resp, err := a.brain.GetImplicitContext(handshakeCtx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to fetch context")
		slog.Warn("context fetch failed, continuing without history", "error", err)
	}

	var contextParts []string
	for _, node := range resp {
		contextParts = append(contextParts, "  "+node)
	}
	contextStr := strings.Join(contextParts, "\n")

	// config
	// TODO: add more voices, with pre-view to main app
	config := &genai.LiveConnectConfig{
		ResponseModalities: []genai.Modality{genai.ModalityAudio},
		SpeechConfig: &genai.SpeechConfig{
			VoiceConfig: &genai.VoiceConfig{
				PrebuiltVoiceConfig: &genai.PrebuiltVoiceConfig{
					// aoede, puck, fenrir, charon, kore
					VoiceName: "Puck",
				},
			},
		},
		Tools: toolDefinitions(),
		SystemInstruction: &genai.Content{
			Role: "system",
			Parts: []*genai.Part{
				{
					Text: fmt.Sprintf("You are Ora, an ambient AI agent and private memory companion. You act as a 'Temporal Brain,' maintaining a high-fidelity understanding of the user's workspace to provide seamless, context-aware assistance while strictly prioritizing privacy and performance.\n\nSystem Environment:\n  OS: %s\n  Arch: %s\n  Shell: %s\n\nThe user's current session context is derived from their local activity tree:\n%s\n\nUse this state to offer precise, technically-grounded help, acknowledging their current focus without being intrusive. Keep responses concise and conversational unless the user asks for detail, optimized for real-time audio interaction. Avoid long lists or markdown formatting.\n\nYou have access to tools. Use shell_exec to run commands when the user asks. Use the correct shell syntax for the user's OS (powershell for windows, sh for linux/mac). Always confirm destructive operations first.", runtime.GOOS, runtime.GOARCH, shellName(), contextStr),
				},
			},
		},
	}

	slog.Debug("connecting to live API")

	session, err := client.Live.Connect(handshakeCtx, model, config)

	slog.Debug("connected to Live API", "error", err)
	if err != nil {
		span.RecordError(err)
		span.End()
		return fmt.Errorf("websocket handshake failed: %w", err)
	}
	defer session.Close()

	// kickstartttterrr
	err = session.SendClientContent(genai.LiveSendClientContentParameters{
		Turns: []*genai.Content{
			{
				Role: "user",
				Parts: []*genai.Part{
					{Text: "Hello Ora. You are online. Greet me briefly."},
				},
			},
		},
	})
	if err != nil {
		span.RecordError(err)
		slog.Error("failed to send initial client turn", "error", err)
	}

	// handshake complete, end span
	span.End()

	errChan := make(chan error, 1) // 1 slot error channel

	// receive loop - handles model responses and tool calls
	go a.receiveLoop(ctx, session, model, errChan)

	micChan, err := a.mic.StartCapture(ctx)
	if err != nil {
		return fmt.Errorf("failed to start microphone: %w", err)
	}

	// audio send loop
	go a.audioSendLoop(ctx, session, micChan, errChan)

	// text send loop (uses textchan)
	go a.textSendLoop(ctx, session)

	select {
	case err := <-errChan:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *Agent) receiveLoop(ctx context.Context, session *genai.Session, model string, errChan chan error) {
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

		// tool calling - check if model wants to use a tool
		if msg.ToolCall != nil {
			_, toolSpan := otelTracer.Start(recvCtx, "Agent.ToolExecution")

			for _, fc := range msg.ToolCall.FunctionCalls {
				slog.Info("tool call received", "tool", fc.Name, "args", fc.Args)
				toolSpan.SetAttributes(attribute.String("tool.name", fc.Name))

				result := a.executeTool(fc.Name, fc.Args)

				// Safety
				// truncate massive results to prevent 1011 crash
				if len(result) > 10000 {
					result = result[:10000] + "\n\n[Output Truncated: Result too large for Live context]"
					slog.Warn("tool result truncated", "tool", fc.Name, "length", len(result))
				}

				slog.Info("tool result", "tool", fc.Name, "result", result)

				// send result back to model with matching ID
				err := session.SendToolResponse(genai.LiveToolResponseInput{
					FunctionResponses: []*genai.FunctionResponse{{
						ID:       fc.ID,
						Name:     fc.Name,
						Response: map[string]any{"output": result},
					}},
				})
				if err != nil {
					toolSpan.RecordError(err)
					slog.Error("failed to send tool response", "error", err)
				}
			}
			toolSpan.End()
		}
	}
}

func (a *Agent) audioSendLoop(ctx context.Context, session *genai.Session, micChan <-chan []byte, errChan chan error) {
	otelTracer := obs.GetTracer(ctx, "ora.agent")
	sendCtx, sendSpan := otelTracer.Start(ctx, "Agent.SendLoop")
	defer sendSpan.End()
	for {
		select {
		case pcm := <-micChan:
			input := genai.LiveRealtimeInput{
				Media: &genai.Blob{
					Data:     pcm,
					MIMEType: "audio/pcm;rate=24000",
				},
			}

			if err := session.SendRealtimeInput(input); err != nil {
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
			err := session.SendClientContent(genai.LiveSendClientContentParameters{
				Turns: []*genai.Content{
					{
						Role: "user",
						Parts: []*genai.Part{
							{Text: text},
						},
					},
				},
			})
			if err != nil {
				slog.Error("failed to send text", "error", err)
			}
		case <-ctx.Done():
			return
		}
	}
}
