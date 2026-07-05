package agent_test

import (
	"context"
	"ora/internal/agent"
	"strings"
	"testing"
)

func TestAgent_VoiceDefaultsEmpty(t *testing.T) {
	a := agent.NewAgent(&mockMic{}, &mockSpeaker{}, &mockBrain{}, nil, "FAKE_API_KEY")
	if got := a.GetVoice(); got != "" {
		t.Errorf("expected a fresh agent to have no voice set, got %q", got)
	}
}

func TestAgent_SetVoiceGetVoiceRoundTrip(t *testing.T) {
	a := agent.NewAgent(&mockMic{}, &mockSpeaker{}, &mockBrain{}, nil, "FAKE_API_KEY")
	a.SetVoice("Kore")
	if got := a.GetVoice(); got != "Kore" {
		t.Errorf("expected GetVoice to return Kore, got %q", got)
	}

	// setting again overwrites cleanly
	a.SetVoice("Zephyr")
	if got := a.GetVoice(); got != "Zephyr" {
		t.Errorf("expected GetVoice to return Zephyr after overwrite, got %q", got)
	}
}

func TestAgent_TriggerReconnectIsNonBlockingAndCoalesces(t *testing.T) {
	a := agent.NewAgent(&mockMic{}, &mockSpeaker{}, &mockBrain{}, nil, "FAKE_API_KEY")

	// first trigger fills the buffered channel
	a.TriggerReconnect()
	// second trigger must not block (channel already has a pending signal)
	done := make(chan struct{})
	go func() {
		a.TriggerReconnect()
		close(done)
	}()
	select {
	case <-done:
	default:
	}
	<-done // if TriggerReconnect blocked, this would hang and the test would time out

	// exactly one signal should be readable
	select {
	case <-a.ReconnectChan:
	default:
		t.Fatal("expected a pending reconnect signal on ReconnectChan")
	}
	select {
	case <-a.ReconnectChan:
		t.Fatal("expected only one coalesced reconnect signal, got a second")
	default:
	}
}

func TestAgent_PreviewVoiceRejectsUnknownVoiceWithoutNetworkCall(t *testing.T) {
	a := agent.NewAgent(&mockMic{}, &mockSpeaker{}, &mockBrain{}, nil, "FAKE_API_KEY")

	err := a.PreviewVoice(context.Background(), "NotARealVoice")
	if err == nil {
		t.Fatal("expected an error for an unknown voice name")
	}
	if !strings.Contains(err.Error(), "NotARealVoice") {
		t.Errorf("expected error to mention the bad voice name, got: %v", err)
	}
}
