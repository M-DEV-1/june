package memory_test

import (
	"context"
	"errors"
	"ora/internal/memory"
	"testing"
)

// fakeGate stands in for the daemon's shared daily-quota gate. err, when set, is what every Allow call returns; calls records every model name Allow was asked about, so a test can check whether the gate was reached at all.
type fakeGate struct {
	err   error
	calls []string
}

func (g *fakeGate) Allow(model string) error {
	g.calls = append(g.calls, model)
	return g.err
}

// TestGeminiSummarizer_ReconcileNotes_GateRefusesWithoutCallingBackend checks that a refusing gate makes ReconcileNotes return the gate's error before ever reaching the Gemini API.
func TestGeminiSummarizer_ReconcileNotes_GateRefusesWithoutCallingBackend(t *testing.T) {
	summarizer, err := memory.NewGeminiSummarizer("fake-key-no-network")
	if err != nil {
		t.Fatalf("NewGeminiSummarizer: %v", err)
	}
	gateErr := errors.New("daily quota reached")
	summarizer.SetRequestGate(&fakeGate{err: gateErr})

	ops, err := summarizer.ReconcileNotes(context.Background(), nil, []string{"user likes tea"})
	if !errors.Is(err, gateErr) {
		t.Fatalf("ReconcileNotes error = %v, want it to wrap the gate's refusal", err)
	}
	if ops != nil {
		t.Errorf("ops = %v, want nil on refusal", ops)
	}
}

// TestGeminiSummarizer_ReconcileNotes_PermittingGateLeavesEmptyShortCircuitUnchanged checks that installing a permitting gate does not disturb the existing empty-candidates short circuit, and that the gate is never even consulted when there is nothing to send.
func TestGeminiSummarizer_ReconcileNotes_PermittingGateLeavesEmptyShortCircuitUnchanged(t *testing.T) {
	summarizer, err := memory.NewGeminiSummarizer("fake-key-no-network")
	if err != nil {
		t.Fatalf("NewGeminiSummarizer: %v", err)
	}
	gate := &fakeGate{}
	summarizer.SetRequestGate(gate)

	ops, err := summarizer.ReconcileNotes(context.Background(), nil, nil)
	if err != nil || ops != nil {
		t.Fatalf("ReconcileNotes(empty) = (%v, %v), want (nil, nil) same as with no gate installed", ops, err)
	}
	if len(gate.calls) != 0 {
		t.Errorf("gate.Allow called %d times for empty candidates, want 0: nothing to send", len(gate.calls))
	}
}

// TestGeminiSummarizer_AttributeThreads_GateRefusesWithoutCallingBackend checks that a refusing gate makes AttributeThreads return the gate's error before ever reaching the Gemini API.
func TestGeminiSummarizer_AttributeThreads_GateRefusesWithoutCallingBackend(t *testing.T) {
	summarizer, err := memory.NewGeminiSummarizer("fake-key-no-network")
	if err != nil {
		t.Fatalf("NewGeminiSummarizer: %v", err)
	}
	gateErr := errors.New("daily quota reached")
	summarizer.SetRequestGate(&fakeGate{err: gateErr})

	attr, err := summarizer.AttributeThreads(context.Background(), nil, nil)
	if !errors.Is(err, gateErr) {
		t.Fatalf("AttributeThreads error = %v, want it to wrap the gate's refusal", err)
	}
	if attr != nil {
		t.Errorf("attr = %v, want nil on refusal", attr)
	}
}

// TestGeminiSummarizer_DeriveState_GateRefusesWithoutCallingBackend checks that a refusing gate makes DeriveState return the gate's error before ever reaching the Gemini API, with no local backend or fallback installed to intercept it.
func TestGeminiSummarizer_DeriveState_GateRefusesWithoutCallingBackend(t *testing.T) {
	summarizer, err := memory.NewGeminiSummarizer("fake-key-no-network")
	if err != nil {
		t.Fatalf("NewGeminiSummarizer: %v", err)
	}
	gateErr := errors.New("daily quota reached")
	summarizer.SetRequestGate(&fakeGate{err: gateErr})

	state, err := summarizer.DeriveState(context.Background(), []string{"did a thing"}, nil)
	if !errors.Is(err, gateErr) {
		t.Fatalf("DeriveState error = %v, want it to wrap the gate's refusal", err)
	}
	if state != "" {
		t.Errorf("state = %q, want empty on refusal", state)
	}
}

// TestGeminiSummarizer_DeriveState_PermittingGateLeavesEmptyShortCircuitUnchanged checks that installing a permitting gate does not disturb the existing empty-input short circuit, and that the gate is never consulted when there is nothing to summarize.
func TestGeminiSummarizer_DeriveState_PermittingGateLeavesEmptyShortCircuitUnchanged(t *testing.T) {
	summarizer, err := memory.NewGeminiSummarizer("fake-key-no-network")
	if err != nil {
		t.Fatalf("NewGeminiSummarizer: %v", err)
	}
	gate := &fakeGate{}
	summarizer.SetRequestGate(gate)

	state, err := summarizer.DeriveState(context.Background(), nil, nil)
	if err != nil || state != "" {
		t.Fatalf("DeriveState(empty) = (%q, %v), want (\"\", nil) same as with no gate installed", state, err)
	}
	if len(gate.calls) != 0 {
		t.Errorf("gate.Allow called %d times for empty input, want 0: nothing to summarize", len(gate.calls))
	}
}

// TestGeminiSummarizer_DeriveState_LocalBackendBypassesTheRequestGate checks that the local-llama-server path (SetStateBackend) still answers even when the metered request gate refuses: the local backend spends no metered quota, so the gate must never be consulted on that path.
func TestGeminiSummarizer_DeriveState_LocalBackendBypassesTheRequestGate(t *testing.T) {
	summarizer, err := memory.NewGeminiSummarizer("fake-key-no-network")
	if err != nil {
		t.Fatalf("NewGeminiSummarizer: %v", err)
	}
	summarizer.SetRequestGate(&fakeGate{err: errors.New("daily quota reached")})
	calls := 0
	summarizer.SetStateBackend(func(ctx context.Context, prompt string) (string, error) {
		calls++
		return "local answer", nil
	})

	state, err := summarizer.DeriveState(context.Background(), []string{"did a thing"}, nil)
	if err != nil {
		t.Fatalf("DeriveState: %v", err)
	}
	if calls != 1 {
		t.Errorf("local backend called %d times, want 1: it must run even though the metered gate refuses", calls)
	}
	if state != "local answer" {
		t.Errorf("state = %q, want the local backend's answer", state)
	}
}

// TestGeminiSummarizer_ConsolidateNotes_GateRefusesWithoutCallingBackend checks that a refusing gate makes ConsolidateNotes return the gate's error before ever reaching the Gemini API.
func TestGeminiSummarizer_ConsolidateNotes_GateRefusesWithoutCallingBackend(t *testing.T) {
	summarizer, err := memory.NewGeminiSummarizer("fake-key-no-network")
	if err != nil {
		t.Fatalf("NewGeminiSummarizer: %v", err)
	}
	gateErr := errors.New("daily quota reached")
	summarizer.SetRequestGate(&fakeGate{err: gateErr})

	merged, err := summarizer.ConsolidateNotes(context.Background(), []string{"user likes tea"})
	if !errors.Is(err, gateErr) {
		t.Fatalf("ConsolidateNotes error = %v, want it to wrap the gate's refusal", err)
	}
	if merged != nil {
		t.Errorf("merged = %v, want nil on refusal", merged)
	}
}

// TestGeminiSummarizer_ConsolidateNotes_PermittingGateLeavesEmptyShortCircuitUnchanged checks that installing a permitting gate does not disturb the existing empty-notes short circuit.
func TestGeminiSummarizer_ConsolidateNotes_PermittingGateLeavesEmptyShortCircuitUnchanged(t *testing.T) {
	summarizer, err := memory.NewGeminiSummarizer("fake-key-no-network")
	if err != nil {
		t.Fatalf("NewGeminiSummarizer: %v", err)
	}
	gate := &fakeGate{}
	summarizer.SetRequestGate(gate)

	merged, err := summarizer.ConsolidateNotes(context.Background(), nil)
	if err != nil || merged != nil {
		t.Fatalf("ConsolidateNotes(empty) = (%v, %v), want (nil, nil) same as with no gate installed", merged, err)
	}
	if len(gate.calls) != 0 {
		t.Errorf("gate.Allow called %d times for empty notes, want 0: nothing to send", len(gate.calls))
	}
}

// TestGeminiSummarizer_AnalyzeScreen_GateRefusesWithoutCallingBackend checks that a refusing gate makes AnalyzeScreen return the zero ScreenSight before ever reaching the Gemini API.
func TestGeminiSummarizer_AnalyzeScreen_GateRefusesWithoutCallingBackend(t *testing.T) {
	summarizer, err := memory.NewGeminiSummarizer("fake-key-no-network")
	if err != nil {
		t.Fatalf("NewGeminiSummarizer: %v", err)
	}
	summarizer.SetRequestGate(&fakeGate{err: errors.New("daily quota reached")})

	sight := summarizer.AnalyzeScreen(context.Background(), []byte{1, 2, 3})
	if sight.UserActivity != "" || len(sight.VisibleText) != 0 || sight.Summary != "" {
		t.Errorf("AnalyzeScreen = %+v, want the zero value on refusal", sight)
	}
}

// TestGeminiSummarizer_AnalyzeScreen_PermittingGateLeavesEmptyShortCircuitUnchanged checks that installing a permitting gate does not disturb the existing empty-png short circuit, and that the gate is never consulted for it.
func TestGeminiSummarizer_AnalyzeScreen_PermittingGateLeavesEmptyShortCircuitUnchanged(t *testing.T) {
	summarizer, err := memory.NewGeminiSummarizer("fake-key-no-network")
	if err != nil {
		t.Fatalf("NewGeminiSummarizer: %v", err)
	}
	gate := &fakeGate{}
	summarizer.SetRequestGate(gate)

	sight := summarizer.AnalyzeScreen(context.Background(), nil)
	if sight.UserActivity != "" || len(sight.VisibleText) != 0 || sight.Summary != "" {
		t.Errorf("AnalyzeScreen(nil) = %+v, want the zero value", sight)
	}
	if len(gate.calls) != 0 {
		t.Errorf("gate.Allow called %d times for empty input, want 0: no screenshot to analyze", len(gate.calls))
	}
}

// TestGeminiSummarizer_DescribeScreen_GateRefusesWithoutCallingBackend checks that DescribeScreen, which flattens AnalyzeScreen's result, also comes back empty under a refusing gate rather than reaching the Gemini API.
func TestGeminiSummarizer_DescribeScreen_GateRefusesWithoutCallingBackend(t *testing.T) {
	summarizer, err := memory.NewGeminiSummarizer("fake-key-no-network")
	if err != nil {
		t.Fatalf("NewGeminiSummarizer: %v", err)
	}
	summarizer.SetRequestGate(&fakeGate{err: errors.New("daily quota reached")})

	desc := summarizer.DescribeScreen(context.Background(), []byte{1, 2, 3})
	if desc != "" {
		t.Errorf("DescribeScreen = %q, want empty on refusal", desc)
	}
}

// TestGeminiSummarizer_Digest_GateRefusesWithoutCallingBackend checks that a refusing gate makes Digest return the gate's error before ever reaching the Gemini API.
func TestGeminiSummarizer_Digest_GateRefusesWithoutCallingBackend(t *testing.T) {
	summarizer, err := memory.NewGeminiSummarizer("fake-key-no-network")
	if err != nil {
		t.Fatalf("NewGeminiSummarizer: %v", err)
	}
	gateErr := errors.New("daily quota reached")
	summarizer.SetRequestGate(&fakeGate{err: gateErr})

	digest, err := summarizer.Digest(context.Background(), "", []string{"fixed the build", "wrote the notes"})
	if !errors.Is(err, gateErr) {
		t.Fatalf("Digest error = %v, want it to wrap the gate's refusal", err)
	}
	if digest != "" {
		t.Errorf("digest = %q, want empty on refusal", digest)
	}
}
