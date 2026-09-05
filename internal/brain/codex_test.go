package brain

import (
	"context"
	"errors"
	"strings"
	"testing"

	"ora/internal/agent"
	"ora/internal/config"

	"google.golang.org/genai"
)

// fakeAsker is a CodexAsker whose answer, or error, is fixed in advance, so FromAsker and FromConfig can be tested without a real ChatGPT login.
type fakeAsker struct {
	trace agent.TurnTrace
	err   error
}

func (f fakeAsker) AskText(ctx context.Context, question string) (agent.TurnTrace, error) {
	return f.trace, f.err
}

// FromAsker must hand back exactly the asker's answer text, and turn a failed or empty-answer ask into an error rather than an empty success.
func TestFromAsker(t *testing.T) {
	if got, err := FromAsker(fakeAsker{trace: agent.TurnTrace{Answer: "the answer"}})(context.Background(), "hi"); err != nil || got != "the answer" {
		t.Fatalf("got %q, err %v, want \"the answer\", nil", got, err)
	}
	if _, err := FromAsker(fakeAsker{err: errors.New("codex login: run `codex login` again")})(context.Background(), "hi"); err == nil || !strings.Contains(err.Error(), "codex login") {
		t.Fatalf("error = %v, want the asker's error to pass through", err)
	}
	if _, err := FromAsker(fakeAsker{trace: agent.TurnTrace{Answer: ""}})(context.Background(), "hi"); err == nil {
		t.Fatal("an empty answer must be an error, not an empty success")
	}
}

// FromConfig must route config.BrainCodex through the asker it is given instead of falling back to the Gemini API, which is the fix for POST /brains {"brain":"codex"} answering as gemini once a caller supplies one.
func TestFromConfig_CodexWithAsker(t *testing.T) {
	cfg := config.BrainConfig{Provider: config.BrainCodex}
	got, err := FromConfig(cfg, "", fakeAsker{trace: agent.TurnTrace{Answer: "from codex"}})(context.Background(), "hi")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "from codex" {
		t.Fatalf("got %q, want %q", got, "from codex")
	}
}

// TestWithCodexFallback_HandsOverOnQuotaAndOverload checks that the two failures no Gemini model can fix — 429 when the day's free-tier requests are spent and 503 when every model is overloaded — send the same prompt to Codex, and that any other failure does not.
func TestWithCodexFallback_HandsOverOnQuotaAndOverload(t *testing.T) {
	cases := []struct {
		name         string
		primaryErr   error
		wantFallback bool
	}{
		{"429 quota exhausted hands over", genai.APIError{Code: 429, Message: "quota"}, true},
		{"503 overloaded hands over", genai.APIError{Code: 503, Message: "overloaded"}, true},
		{"400 bad request does not hand over", genai.APIError{Code: 400, Message: "bad"}, false},
		{"a plain error does not hand over", errors.New("no network"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPrompt string
			primary := func(ctx context.Context, prompt string) (string, error) { return "", tc.primaryErr }
			fallback := func(ctx context.Context, prompt string) (string, error) {
				gotPrompt = prompt
				return "codex answered", nil
			}

			reply, err := WithCodexFallback(primary, fallback)(context.Background(), "the prompt")

			if tc.wantFallback {
				if err != nil {
					t.Fatalf("err = %v, want nil once Codex answered", err)
				}
				if reply != "codex answered" {
					t.Errorf("reply = %q, want the fallback's answer", reply)
				}
				if gotPrompt != "the prompt" {
					t.Errorf("fallback saw prompt %q, want the original %q", gotPrompt, "the prompt")
				}
				return
			}
			if gotPrompt != "" {
				t.Error("fallback ran for a failure Codex cannot fix either, want it left alone")
			}
			if err == nil || err.Error() != tc.primaryErr.Error() {
				t.Errorf("err = %v, want the primary's error %v passed through", err, tc.primaryErr)
			}
		})
	}
}

// TestWithCodexFallback_SuccessNeverReachesTheFallback checks that a working primary is passed straight through and costs no Codex call.
func TestWithCodexFallback_SuccessNeverReachesTheFallback(t *testing.T) {
	called := false
	primary := func(ctx context.Context, prompt string) (string, error) { return "gemini answered", nil }
	fallback := func(ctx context.Context, prompt string) (string, error) {
		called = true
		return "codex answered", nil
	}

	reply, err := WithCodexFallback(primary, fallback)(context.Background(), "p")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if reply != "gemini answered" {
		t.Errorf("reply = %q, want the primary's answer", reply)
	}
	if called {
		t.Error("fallback ran even though the primary succeeded")
	}
}

// TestWithCodexFallback_NoFallbackConfigured checks that a nil fallback — no ChatGPT login on this machine — leaves the primary's error exactly as it was rather than turning it into a different one.
func TestWithCodexFallback_NoFallbackConfigured(t *testing.T) {
	quota := genai.APIError{Code: 429, Message: "quota"}
	primary := func(ctx context.Context, prompt string) (string, error) { return "", quota }

	var apiErr genai.APIError
	if _, err := WithCodexFallback(primary, nil)(context.Background(), "p"); !errors.As(err, &apiErr) || apiErr.Code != 429 {
		t.Errorf("err = %v, want the primary's 429 passed through", err)
	}
}

// TestWithCodexFallback_FallbackFailureReportsBoth checks that when Codex fails too the caller is told the original quota failure as well, since that is the one that explains the day.
func TestWithCodexFallback_FallbackFailureReportsBoth(t *testing.T) {
	primary := func(ctx context.Context, prompt string) (string, error) {
		return "", genai.APIError{Code: 429, Message: "quota"}
	}
	fallback := func(ctx context.Context, prompt string) (string, error) {
		return "", errors.New("not logged in")
	}

	_, err := WithCodexFallback(primary, fallback)(context.Background(), "p")
	if err == nil {
		t.Fatal("err = nil, want a failure")
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("err = %v, want it to name the fallback's failure", err)
	}
}
