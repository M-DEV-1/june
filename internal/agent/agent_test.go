package agent_test

import (
	"context"
	"ora/internal/agent"
	"testing"
	"time"
)

// mock hardware & brain

type mockMic struct{}

func (m *mockMic) StartCapture(ctx context.Context) (<-chan []byte, error) { return nil, nil }
func (m *mockMic) Close() error                                            { return nil }
func (m *mockMic) CurrentAmplitude() float64                               { return 0 }

type mockSpeaker struct{}

func (s *mockSpeaker) Play(pcm []byte) error { return nil }
func (s *mockSpeaker) Flush()                {}
func (s *mockSpeaker) Close() error          { return nil }
func (s *mockSpeaker) CurrentAmplitude() float64 { return 0 }

type mockBrain struct{}

func (b *mockBrain) GetImplicitContext(ctx context.Context) ([]string, error) { return nil, nil }
func (b *mockBrain) QueryMemory(ctx context.Context, query string) ([]string, error) { return nil, nil }

// behavior

func TestAgent_ConnectFailsWithBadKey(t *testing.T) {
	// inject mocks and fake API key
	a := agent.NewAgent(&mockMic{}, &mockSpeaker{}, &mockBrain{}, nil, "FAKE_API_KEY")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// connection failure to gemini
	err := a.Connect(ctx)

	if err == nil {
		t.Fatal("Expected Connect to fail with a fake API key, but it succeeded..")
	}

	t.Logf("Successfully caught API connection error: %+v", err)
}
