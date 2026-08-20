package agent

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"time"

	"ora/internal/config"

	"google.golang.org/genai"
)

// Channel is which Gemini surface a companion turn used.
type Channel string

const (
	// ChannelText is Models.GenerateContent against config.TextModel.
	ChannelText Channel = "text"
	// ChannelVoice is the Live API against config.VoiceModel, matching production voice (handshake frozen, no per-turn RetrieveRelevant).
	ChannelVoice Channel = "voice"
)

// maxAskIterations bounds tool-call round trips on both AskText and AskVoice so a runaway loop can't hang an eval.
const maxAskIterations = 6

// ToolHop is one model-initiated tool call plus the string we sent back.
type ToolHop struct {
	Name   string
	Args   map[string]any
	Result string
}

// TurnTrace is everything needed to study why a companion answer happened: frozen handshake, optional per-turn inject, thoughts, tool hops, and the final reply.
type TurnTrace struct {
	Channel   Channel
	Model     string
	Question  string
	Handshake []string
	Injected  []string
	Thoughts  []string
	ToolHops  []ToolHop
	Answer    string
	Duration  time.Duration
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
	instruction = systemInstructionText(now, runtime.GOOS, runtime.GOARCH, shellName(), strings.Join(contextParts, "\n"), toolsCount)
	return instruction, contextParts
}

// AskText runs one question through GenerateContent (config.TextModel) with the same handshake context Live gets, per-turn RetrieveRelevant like a typed turn, memory tools, and thoughts captured. Input: question text. Output: TurnTrace including thoughts and tool hops.
func (a *Agent) AskText(ctx context.Context, question string) (TurnTrace, error) {
	start := time.Now()
	now := start
	instruction, handshake := a.HandshakePrompt(ctx, now)

	recallCtx, cancel := context.WithTimeout(ctx, textSendLoopRetrieveTimeout)
	injected, err := a.brain.RetrieveRelevant(recallCtx, question, 2)
	cancel()
	if err != nil {
		slog.Warn("AskText retrieve relevant failed, continuing without inject", "error", err)
		injected = nil
	}

	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  a.apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return TurnTrace{}, fmt.Errorf("ask text: client: %w", err)
	}

	contents := buildTurnContent(now, injected, question)
	cfg := &genai.GenerateContentConfig{
		SystemInstruction: &genai.Content{Role: "system", Parts: []*genai.Part{{Text: instruction}}},
		Tools:             evalTools(),
		ThinkingConfig:    thinkingConfig(),
	}

	tr := TurnTrace{
		Channel:   ChannelText,
		Model:     config.TextModel,
		Question:  question,
		Handshake: handshake,
		Injected:  injected,
	}

	for i := 0; i < maxAskIterations; i++ {
		resp, err := client.Models.GenerateContent(ctx, config.TextModel, contents, cfg)
		if err != nil {
			tr.Duration = time.Since(start)
			return tr, fmt.Errorf("ask text: generate (iteration %d): %w", i, err)
		}
		if len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil {
			tr.Duration = time.Since(start)
			return tr, fmt.Errorf("ask text: empty response (iteration %d)", i)
		}
		contents = append(contents, resp.Candidates[0].Content)
		collectParts(resp.Candidates[0].Content.Parts, &tr)

		calls := resp.FunctionCalls()
		if len(calls) == 0 {
			tr.Duration = time.Since(start)
			return tr, nil
		}
		var parts []*genai.Part
		for _, fc := range calls {
			result := a.evalExecute(ctx, fc.Name, fc.Args)
			tr.ToolHops = append(tr.ToolHops, ToolHop{Name: fc.Name, Args: fc.Args, Result: result})
			parts = append(parts, genai.NewPartFromFunctionResponse(fc.Name, map[string]any{"output": result}))
		}
		contents = append(contents, genai.NewContentFromParts(parts, genai.RoleUser))
	}
	tr.Duration = time.Since(start)
	return tr, fmt.Errorf("ask text: exceeded %d iterations without a final answer", maxAskIterations)
}

// AskVoice runs one question through the Live API (config.VoiceModel) as a text client turn, matching production voice: handshake context only, no RetrieveRelevant injection. Input: question text. Output: TurnTrace from output transcription, thought parts, and tool hops.
func (a *Agent) AskVoice(ctx context.Context, question string) (TurnTrace, error) {
	start := time.Now()
	now := start
	instruction, handshake := a.HandshakePrompt(ctx, now)

	tr := TurnTrace{
		Channel:   ChannelVoice,
		Model:     config.VoiceModel,
		Question:  question,
		Handshake: handshake,
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
		Tools:                    evalTools(),
		SystemInstruction: &genai.Content{
			Role:  "system",
			Parts: []*genai.Part{{Text: instruction}},
		},
	}

	session, err := client.Live.Connect(ctx, config.VoiceModel, cfg)
	if err != nil {
		return tr, fmt.Errorf("ask voice: live connect: %w", err)
	}
	defer session.Close()

	if err := session.SendClientContent(genai.LiveSendClientContentParameters{
		Turns: buildTurnContent(now, nil, question),
	}); err != nil {
		tr.Duration = time.Since(start)
		return tr, fmt.Errorf("ask voice: send: %w", err)
	}

	var answer strings.Builder
	toolRounds := 0
	for {
		if ctx.Err() != nil {
			tr.Answer = strings.TrimSpace(answer.String())
			tr.Duration = time.Since(start)
			if tr.Answer != "" {
				return tr, nil
			}
			return tr, ctx.Err()
		}
		msg, err := session.Receive()
		if err != nil {
			tr.Answer = strings.TrimSpace(answer.String())
			tr.Duration = time.Since(start)
			if tr.Answer != "" {
				return tr, nil
			}
			return tr, fmt.Errorf("ask voice: receive: %w", err)
		}
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
			if toolRounds > maxAskIterations {
				tr.Answer = strings.TrimSpace(answer.String())
				tr.Duration = time.Since(start)
				return tr, fmt.Errorf("ask voice: exceeded %d tool rounds", maxAskIterations)
			}
			var responses []*genai.FunctionResponse
			for _, fc := range msg.ToolCall.FunctionCalls {
				result := a.evalExecute(ctx, fc.Name, fc.Args)
				tr.ToolHops = append(tr.ToolHops, ToolHop{Name: fc.Name, Args: fc.Args, Result: result})
				responses = append(responses, &genai.FunctionResponse{
					ID:       fc.ID,
					Name:     fc.Name,
					Response: map[string]any{"output": result},
				})
			}
			if err := session.SendToolResponse(genai.LiveSendToolResponseParameters{FunctionResponses: responses}); err != nil {
				tr.Answer = strings.TrimSpace(answer.String())
				tr.Duration = time.Since(start)
				return tr, fmt.Errorf("ask voice: tool response: %w", err)
			}
			continue
		}
		if msg.ServerContent != nil && (msg.ServerContent.TurnComplete || msg.ServerContent.GenerationComplete) {
			tr.Answer = strings.TrimSpace(answer.String())
			tr.Duration = time.Since(start)
			return tr, nil
		}
	}
}

func evalTools() []*genai.Tool {
	tools := subtaskTools()
	tools = append(tools, &genai.Tool{GoogleSearch: &genai.GoogleSearch{}})
	return tools
}

func (a *Agent) evalExecute(ctx context.Context, name string, args map[string]any) string {
	if !subtaskAllowedTools[name] {
		return fmt.Sprintf("error: tool %q is disabled in evals (read-only memory eval)", name)
	}
	return a.executeTool(ctx, name, args)
}

func collectParts(parts []*genai.Part, tr *TurnTrace) {
	var answer strings.Builder
	if tr.Answer != "" {
		answer.WriteString(tr.Answer)
	}
	for _, p := range parts {
		if p == nil || p.Text == "" {
			continue
		}
		if p.Thought {
			tr.Thoughts = append(tr.Thoughts, p.Text)
			continue
		}
		answer.WriteString(p.Text)
	}
	tr.Answer = strings.TrimSpace(answer.String())
}
