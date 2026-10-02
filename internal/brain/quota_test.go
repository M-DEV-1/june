package brain

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"june/internal/agent"
	"june/internal/config"

	"google.golang.org/genai"
)

// alwaysOK is a Brain that always answers "ok" and never fails, used to exercise WithDailyQuota without a real model.
func alwaysOK(ctx context.Context, prompt string) (string, error) {
	return "ok", nil
}

// TestWithDailyQuota_BackgroundReservesShareForAsks checks that a background-tagged wrapper (forAsks false) is capped at limit-minus-reserved while an asks-tagged wrapper on the same model and the same shared state can still spend the reserved share, and that the refusal at the full limit is one the ask's hand-over recognises.
func TestWithDailyQuota_BackgroundReservesShareForAsks(t *testing.T) {
	state := NewQuotaState(t.TempDir())
	opts := QuotaOptions{"gemini-3.5-flash-lite": {Limit: 5, Reserved: 2}}
	background := WithDailyQuota(state, "gemini-3.5-flash-lite", false, opts, alwaysOK)
	asks := WithDailyQuota(state, "gemini-3.5-flash-lite", true, opts, alwaysOK)

	// Background may spend only 3 of the 5 (5 - 2 reserved).
	for i := 0; i < 3; i++ {
		if _, err := background(context.Background(), "q"); err != nil {
			t.Fatalf("background call %d: unexpected error: %v", i, err)
		}
	}
	var q *ErrDailyQuota
	if _, err := background(context.Background(), "q"); !errors.As(err, &q) {
		t.Fatalf("background call 4: got err %v, want *ErrDailyQuota once its share is spent", err)
	}
	// The 2 remaining requests are still there for asks, since the two wrappers share one counter.
	for i := 0; i < 2; i++ {
		if _, err := asks(context.Background(), "q"); err != nil {
			t.Fatalf("ask call %d: unexpected error: %v", i, err)
		}
	}
	_, err := asks(context.Background(), "q")
	if !errors.As(err, &q) {
		t.Fatalf("ask call 3: got err %v, want *ErrDailyQuota once the full daily limit is spent", err)
	}
	// The refusal must read as Gemini refusing, so an ask hands over to Codex and Claude the same way it does on a real 429.
	if !agent.GeminiCannotAnswer(err) {
		t.Errorf("agent.GeminiCannotAnswer(%v) = false, want the ask handed over", err)
	}
}

// TestGeminiModelFor checks that every provider FromConfig does not send to the Gemini API is reported as not-Gemini, and that an explicit or empty Gemini provider resolves to its own model or config.TextModel. Codex and Ollama are in the first group now that FromConfig fails them with ErrNoBackend rather than answering on Gemini, so a refused call must not spend a slot in a Gemini model's daily count.
func TestGeminiModelFor(t *testing.T) {
	cases := []struct {
		name      string
		cfg       config.BrainConfig
		wantModel string
		wantOK    bool
	}{
		{"codex is not metered as gemini", config.BrainConfig{Provider: config.BrainCodex, Model: "gpt-5.5"}, "", false},
		{"empty provider defaults to TextModel", config.BrainConfig{}, config.TextModel, true},
		{"explicit gemini model", config.BrainConfig{Provider: config.BrainGeminiAPI, Model: "gemini-3.5-flash-lite"}, "gemini-3.5-flash-lite", true},
		// The 2026-09-05 config named a non-Gemini model; the fallback passed that model name to the Gemini SDK and every retry came back "models/gpt-5.5 is not found for API version v1beta".
		{"an unknown provider's model name never becomes a gemini model name", config.BrainConfig{Provider: "made-up", Model: "gpt-5.5"}, config.TextModel, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			model, ok := GeminiModelFor(c.cfg)
			if model != c.wantModel || ok != c.wantOK {
				t.Fatalf("GeminiModelFor(%+v) = (%q, %v), want (%q, %v)", c.cfg, model, ok, c.wantModel, c.wantOK)
			}
		})
	}
}

// TestMetered_GatesGeminiButNotCLI checks the daemon's one-line call site end to end: a Gemini-routed config is refused once the day's cap is already spent, while a Claude CLI config with the same model cap configured under its own name is never touched by the gate at all.
func TestMetered_GatesGeminiButNotCLI(t *testing.T) {
	dir := t.TempDir()
	opts := QuotaOptions{config.TextModel: {Limit: 1, Reserved: 0}}
	// The day's one request is spent already, so the gate has something to refuse without needing a call that reaches Google.
	spent := fmt.Sprintf(`{%q:{%q:1}}`, QuotaDay(), config.TextModel)
	if err := os.WriteFile(filepath.Join(dir, "brain_quota.json"), []byte(spent), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	state := NewQuotaState(dir)

	var q *ErrDailyQuota
	gemini := Metered(config.BrainConfig{}, "", state, true, opts)
	if _, err := gemini(context.Background(), "q"); !errors.As(err, &q) {
		t.Fatalf("got err %v, want *ErrDailyQuota now that the cap of 1 is spent", err)
	}

	claude := Metered(config.BrainConfig{Provider: config.BrainClaudeCLI, Binary: "/nonexistent-claude"}, "", state, true, opts)
	for i := 0; i < 3; i++ {
		if _, err := claude(context.Background(), "q"); errors.As(err, &q) {
			t.Fatalf("claude-cli call %d was refused by the gemini quota gate: %v", i, err)
		}
	}
}

// TestWithDailyQuota_CountsOnlyCallsThatReachedGoogle checks the slot a call reserved is handed back when the failure happened on this machine — no API key, a dead network — and kept when Google answered, even with a refusal or an empty answer. The count is meant to mirror what Google could bill, and an offline laptop used to spend the whole day's allowance on calls that never left it.
func TestWithDailyQuota_CountsOnlyCallsThatReachedGoogle(t *testing.T) {
	tests := []struct {
		name    string
		ret     error
		counted bool
	}{
		{"no API key", fmt.Errorf("no GEMINI_API_KEY: %w", ErrLocalFailure), false},
		{"a dead network", fmt.Errorf("generate: %w", &net.OpError{Op: "dial", Err: errors.New("no route to host")}), false},
		{"an answer", nil, true},
		{"a refusal from Google", fmt.Errorf("generate: %w", genai.APIError{Code: 400, Message: "bad request"}), true},
		{"an answer with no text", errors.New("gemini returned no text"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := NewQuotaState(t.TempDir())
			opts := QuotaOptions{"gemini-3.5-flash": {Limit: 1, Reserved: 0}}
			b := WithDailyQuota(state, "gemini-3.5-flash", true, opts, func(context.Context, string) (string, error) { return "", tt.ret })
			b(context.Background(), "q")
			var q *ErrDailyQuota
			_, err := b(context.Background(), "q")
			if refused := errors.As(err, &q); refused != tt.counted {
				t.Errorf("second call refused = %v, want %v", refused, tt.counted)
			}
		})
	}
}

// TestWithDailyQuota_MetersAnUnlistedGeminiModel checks the default ceiling is actually enforced rather than only reported.
func TestWithDailyQuota_MetersAnUnlistedGeminiModel(t *testing.T) {
	state := NewQuotaState(t.TempDir())
	b := WithDailyQuota(state, "gemini-9-imaginary", true, DefaultQuotaOptions(), alwaysOK)
	for i := 0; i < DefaultQuotaLimit.Limit; i++ {
		if _, err := b(context.Background(), "q"); err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
	}
	var q *ErrDailyQuota
	if _, err := b(context.Background(), "q"); !errors.As(err, &q) {
		t.Fatalf("an unlisted gemini model was never capped: %v", err)
	}
}

// TestQuotaState_ATornWriteDoesNotResetTheCount checks the counter file is replaced by a rename rather than truncated in place: a crash mid-write leaves the leftover temporary file, and the real file must still hold the day's count. Without this a torn write read as "meter from zero" and handed the whole day's allowance back.
func TestQuotaState_ATornWriteDoesNotResetTheCount(t *testing.T) {
	dir := t.TempDir()
	state := NewQuotaState(dir)
	opts := QuotaOptions{"gemini-3.5-flash": {Limit: 5, Reserved: 0}}
	b := WithDailyQuota(state, "gemini-3.5-flash", true, opts, alwaysOK)
	for i := 0; i < 5; i++ {
		if _, err := b(context.Background(), "q"); err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
	}
	// A crash between the temporary file and the rename leaves exactly this behind.
	if err := os.WriteFile(filepath.Join(dir, "brain_quota.json.tmp"), []byte(`{"2026-`), 0o600); err != nil {
		t.Fatalf("write the torn file: %v", err)
	}
	var q *ErrDailyQuota
	if _, err := WithDailyQuota(NewQuotaState(dir), "gemini-3.5-flash", true, opts, alwaysOK)(context.Background(), "q"); !errors.As(err, &q) {
		t.Fatalf("the day's count was lost to a torn write: %v", err)
	}
}
