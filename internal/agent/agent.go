package agent

import (
	"context"
	"fmt"
	"ora/internal/audio"
	"strings"

	"google.golang.org/genai"
)

// read-only interface to sqlite db
type ContextReader interface {
	GetImplicitContext(ctx context.Context) ([]string, error)
}

type Agent struct {
	mic     audio.Microphone
	speaker audio.Speaker
	brain   ContextReader
	apiKey  string
}

// initializer and orchestrates all hardware (2) and memory (1) moduels
func NewAgent(mic audio.Microphone, speaker audio.Speaker, brain ContextReader, apiKey string) *Agent {
	return &Agent{
		mic:     mic,
		speaker: speaker,
		brain:   brain,
		apiKey:  apiKey,
	}
}

func (a *Agent) Connect(ctx context.Context) error {
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  a.apiKey,
		Backend: genai.BackendGeminiAPI,
		HTTPOptions: genai.HTTPOptions{
			APIVersion: "v1alpha",
		},
	})
	if err != nil {
		return fmt.Errorf("failed to initialize genai client: %w", err)
	}

	// drumrolllllll
	// connectttt to livee apiii
	model := "models/gemini-2.5-flash-native-audio-latest"

	resp, err := a.brain.GetImplicitContext(ctx)
	if err != nil {
		fmt.Printf("context fetch failed?: %v\nContinuing..", err)
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
		SystemInstruction: &genai.Content{
			Role: "system",
			Parts: []*genai.Part{
				{
					Text: fmt.Sprintf("You are Ora, an ambient AI agent and private memory companion. You act as a 'Temporal Brain,' maintaining a high-fidelity understanding of the user's workspace to provide seamless, context-aware assistance while strictly prioritizing privacy and performance.\n\nThe user's current session context is derived from their local activity tree:\n%s\n\nUse this state to offer precise, technically-grounded help, acknowledging their current focus without being intrusive. Keep responses concise and conversational unless the user asks for detail, optimized for real-time audio interaction. Avoid long lists or markdown formatting.", contextStr),
				},
			},
		},
	}

	fmt.Println("[DEBUG] connecting to live API...")

	session, err := client.Live.Connect(ctx, model, config)

	fmt.Println("[DEBUG] connected!", err)
	if err != nil {
		return fmt.Errorf("websocket handshake failed: %w", err)
	}
	defer session.Close()

	// Kickstart the conversation
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
		fmt.Printf("[SEND] failed to send initial client turn: %v\n", err)
	}

	errChan := make(chan error, 1) // 1 slot error channel

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			msg, err := session.Receive()
			if err != nil {
				fmt.Printf("\n[RECV] Error: %v\n", err)
				errChan <- fmt.Errorf("receive loop error: %w", err)
				return
			}

			if msg.ServerContent != nil && msg.ServerContent.ModelTurn != nil {
				for _, part := range msg.ServerContent.ModelTurn.Parts {
					if part.Text != "" {
						fmt.Printf("\n[ORA]: %s\n", part.Text)
					}
					// if part is audio
					if part.InlineData != nil {
						// pass directly to our audio pipe
						a.speaker.Play(part.InlineData.Data)
					}
				}
			}
		}
	}()

	micChan, err := a.mic.StartCapture(ctx)
	if err != nil {
		return fmt.Errorf("failed to start microphone: %w", err)
	}

	go func() {
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
					errChan <- fmt.Errorf("failed to send audio: %w", err)
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	select {
	case err := <-errChan:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
