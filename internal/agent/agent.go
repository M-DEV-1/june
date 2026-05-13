package agent

import (
	"context"
	"fmt"
	"ora/internal/audio"

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
	})
	if err != nil {
		return fmt.Errorf("failed to initialize genai client: %w", err)
	}

	// drumrolllllll
	// connectttt to livee apiii
	model := "models/gemini-2.5-flash-native-audio-latest"

	session, err := client.Live.Connect(ctx, model, &genai.LiveConnectConfig{})
	if err != nil {
		return fmt.Errorf("websocket handshake failed: %w", err)
	}
	defer session.Close()

	errChan := make(chan error, 1) // 1 slot error channel

	go func() {
		for {
			msg, err := session.Receive()
			if err != nil {
				errChan <- fmt.Errorf("receive loop error: %w", err)
				return
			}

			if msg.ServerContent != nil && msg.ServerContent.ModelTurn != nil {
				for _, part := range msg.ServerContent.ModelTurn.Parts {
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
