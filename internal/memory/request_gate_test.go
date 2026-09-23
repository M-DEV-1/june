package memory_test

import (
	"context"
	"errors"
	"ora/internal/config"
	"ora/internal/memory"
	"testing"
)

// fakeGate stands in for the daemon's shared daily-quota gate. err, when set, is what every Allow call returns.
type fakeGate struct {
	err error
}

func (g *fakeGate) Allow(model string) error {
	return g.err
}

// Every method that reaches the metered Gemini API must check the request gate first and return its refusal, untouched, without ever reaching the backend.
func TestGeminiSummarizer_GateRefusesWithoutCallingBackend(t *testing.T) {
	gateErr := errors.New("daily quota reached")
	cases := []struct {
		name string
		call func(t *testing.T, s *memory.GeminiSummarizer)
	}{
		{"ReconcileNotes", func(t *testing.T, s *memory.GeminiSummarizer) {
			ops, err := s.ReconcileNotes(context.Background(), nil, []string{"user likes tea"})
			if !errors.Is(err, gateErr) {
				t.Fatalf("ReconcileNotes error = %v, want it to wrap the gate's refusal", err)
			}
			if ops != nil {
				t.Errorf("ops = %v, want nil on refusal", ops)
			}
		}},
		{"AttributeThreads", func(t *testing.T, s *memory.GeminiSummarizer) {
			attr, err := s.AttributeThreads(context.Background(), nil, nil)
			if !errors.Is(err, gateErr) {
				t.Fatalf("AttributeThreads error = %v, want it to wrap the gate's refusal", err)
			}
			if attr != nil {
				t.Errorf("attr = %v, want nil on refusal", attr)
			}
		}},
		{"DeriveState, with no local backend or fallback installed to intercept it", func(t *testing.T, s *memory.GeminiSummarizer) {
			state, err := s.DeriveState(context.Background(), []string{"did a thing"}, nil)
			if !errors.Is(err, gateErr) {
				t.Fatalf("DeriveState error = %v, want it to wrap the gate's refusal", err)
			}
			if state != "" {
				t.Errorf("state = %q, want empty on refusal", state)
			}
		}},
		{"ConsolidateNotes", func(t *testing.T, s *memory.GeminiSummarizer) {
			merged, err := s.ConsolidateNotes(context.Background(), []string{"user likes tea"})
			if !errors.Is(err, gateErr) {
				t.Fatalf("ConsolidateNotes error = %v, want it to wrap the gate's refusal", err)
			}
			if merged != nil {
				t.Errorf("merged = %v, want nil on refusal", merged)
			}
		}},
		{"AnalyzeScreen, returning the zero ScreenSight", func(t *testing.T, s *memory.GeminiSummarizer) {
			sight := s.AnalyzeScreen(context.Background(), []byte{1, 2, 3})
			if sight.UserActivity != "" || len(sight.VisibleText) != 0 || sight.Summary != "" {
				t.Errorf("AnalyzeScreen = %+v, want the zero value on refusal", sight)
			}
		}},
		{"DescribeScreen, which flattens AnalyzeScreen's result", func(t *testing.T, s *memory.GeminiSummarizer) {
			desc := s.DescribeScreen(context.Background(), []byte{1, 2, 3})
			if desc != "" {
				t.Errorf("DescribeScreen = %q, want empty on refusal", desc)
			}
		}},
		{"Digest", func(t *testing.T, s *memory.GeminiSummarizer) {
			digest, err := s.Digest(context.Background(), "", []string{"fixed the build", "wrote the notes"})
			if !errors.Is(err, gateErr) {
				t.Fatalf("Digest error = %v, want it to wrap the gate's refusal", err)
			}
			if digest != "" {
				t.Errorf("digest = %q, want empty on refusal", digest)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			summarizer, err := memory.NewGeminiSummarizer("fake-key-no-network")
			if err != nil {
				t.Fatalf("NewGeminiSummarizer: %v", err)
			}
			summarizer.SetRequestGate(&fakeGate{err: gateErr})
			c.call(t, summarizer)
		})
	}
}

// The local-llama-server path (SetStateBackend) must still answer even when the metered request gate refuses: the local backend spends no metered quota, so the gate must never be consulted on that path. This needs its own gated fake wiring, so it does not fit the tables above.
func TestGeminiSummarizer_DeriveState_LocalBackendBypassesTheRequestGate(t *testing.T) {
	summarizer, err := memory.NewGeminiSummarizer("fake-key-no-network")
	if err != nil {
		t.Fatalf("NewGeminiSummarizer: %v", err)
	}
	summarizer.SetRequestGate(&fakeGate{err: errors.New("daily quota reached")})
	calls := 0
	summarizer.SetJobBackend(config.JobWorkingState, func(ctx context.Context, prompt string) (string, error) {
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
