package brain

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ora/internal/agent"
	"ora/internal/config"

	"google.golang.org/genai"
)

// alwaysOK is a Brain that always answers "ok" and never fails, used to exercise WithDailyQuota without a real model.
func alwaysOK(ctx context.Context, prompt string) (string, error) {
	return "ok", nil
}

// TestWithDailyQuota_RefusesOnceCapReached checks that the wrapped brain answers normally up to the cap and then fails with ErrDailyQuota, without ever calling primary again.
func TestWithDailyQuota_RefusesOnceCapReached(t *testing.T) {
	state := NewQuotaState(t.TempDir())
	opts := QuotaOptions{"gemini-3.5-flash": {Limit: 2, Reserved: 0}}
	calls := 0
	counting := func(ctx context.Context, prompt string) (string, error) {
		calls++
		return "ok", nil
	}
	b := WithDailyQuota(state, "gemini-3.5-flash", true, opts, counting)

	for i := 0; i < 2; i++ {
		if _, err := b(context.Background(), "q"); err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
	}
	var q *ErrDailyQuota
	if _, err := b(context.Background(), "q"); !errors.As(err, &q) {
		t.Fatalf("call 3: got err %v, want *ErrDailyQuota", err)
	}
	if calls != 2 {
		t.Fatalf("primary was called %d times, want 2 — the third call must never reach it", calls)
	}
}

// TestWithDailyQuota_BackgroundReservesShareForAsks checks that a background-tagged wrapper (forAsks false) is capped at limit-minus-reserved while an asks-tagged wrapper on the same model and the same shared state can still spend the reserved share.
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
	if _, err := asks(context.Background(), "q"); !errors.As(err, &q) {
		t.Fatalf("ask call 3: got err %v, want *ErrDailyQuota once the full daily limit is spent", err)
	}
}

// TestWithDailyQuota_UnconfiguredModelPassesThrough checks that a model absent from opts is left unmetered, so turning the gate on cannot silently start refusing a provider it was never told about.
func TestWithDailyQuota_UnconfiguredModelPassesThrough(t *testing.T) {
	state := NewQuotaState(t.TempDir())
	b := WithDailyQuota(state, "some-other-model", true, QuotaOptions{}, alwaysOK)
	for i := 0; i < 100; i++ {
		if _, err := b(context.Background(), "q"); err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
	}
}

// TestWithDailyQuota_PersistsAcrossState checks that the count survives a fresh QuotaState pointed at the same file, the way a daemon restart would see it.
func TestWithDailyQuota_PersistsAcrossState(t *testing.T) {
	dir := t.TempDir()
	opts := QuotaOptions{"gemini-3.5-flash": {Limit: 1, Reserved: 0}}
	if _, err := WithDailyQuota(NewQuotaState(dir), "gemini-3.5-flash", true, opts, alwaysOK)(context.Background(), "q"); err != nil {
		t.Fatalf("first call: unexpected error: %v", err)
	}
	var q *ErrDailyQuota
	if _, err := WithDailyQuota(NewQuotaState(dir), "gemini-3.5-flash", true, opts, alwaysOK)(context.Background(), "q"); !errors.As(err, &q) {
		t.Fatalf("second call on a fresh state over the same dir: got err %v, want *ErrDailyQuota", err)
	}
}

// TestErrDailyQuota_SatisfiesGeminiCannotAnswer checks that the fallback chain agent.AskText already uses for a real 429 (hand over to Codex, then Claude) also fires for ErrDailyQuota, since a request refused before it ever reached Gemini needs the same hand-over as one Gemini itself refused.
func TestErrDailyQuota_SatisfiesGeminiCannotAnswer(t *testing.T) {
	err := &ErrDailyQuota{Model: "gemini-3.5-flash", Limit: 20}
	if !agent.GeminiCannotAnswer(err) {
		t.Fatalf("agent.GeminiCannotAnswer(%v) = false, want true", err)
	}
	var apiErr genai.APIError
	if !errors.As(error(err), &apiErr) || apiErr.Code != 429 {
		t.Fatalf("errors.As did not unwrap to a 429 genai.APIError, got %+v", apiErr)
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
		{"claude cli", config.BrainConfig{Provider: config.BrainClaudeCLI, Model: "sonnet"}, "", false},
		{"agy cli", config.BrainConfig{Provider: config.BrainAgyCLI}, "", false},
		{"grok cli", config.BrainConfig{Provider: config.BrainGrokCLI}, "", false},
		{"empty provider defaults to TextModel", config.BrainConfig{}, config.TextModel, true},
		{"explicit gemini model", config.BrainConfig{Provider: config.BrainGeminiAPI, Model: "gemini-3.5-flash-lite"}, "gemini-3.5-flash-lite", true},
		{"codex with no asker is not metered as gemini", config.BrainConfig{Provider: config.BrainCodex}, "", false},
		// The 2026-09-05 config had provider "codex-direct" with model "gpt-5.5"; the meeting summariser passed that model name to the Gemini SDK and every retry came back "models/gpt-5.5 is not found for API version v1beta".
		{"codex model name never becomes a gemini model name", config.BrainConfig{Provider: config.BrainCodex, Model: "gpt-5.5"}, "", false},
		{"ollama model name never becomes a gemini model name", config.BrainConfig{Provider: config.BrainOllama, Model: "llama3.1:8b"}, "", false},
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
	spent := fmt.Sprintf(`{%q:{%q:1}}`, time.Now().Format("2006-01-02"), config.TextModel)
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

// TestNewQuotaState_FileUnderDataDir pins the counter file's location under the data dir so a reviewer can find it on disk; the reads and writes through that path are exercised indirectly by the tests above.
func TestNewQuotaState_FileUnderDataDir(t *testing.T) {
	dir := t.TempDir()
	state := NewQuotaState(dir)
	if got, want := state.path, filepath.Join(dir, "brain_quota.json"); got != want {
		t.Fatalf("quota file path = %q, want %q", got, want)
	}
}

// TestWithDailyQuota_RefundsACallThatNeverReachedGoogle checks that a failure on this machine — no API key, a dead network, a cancelled context — hands back the slot it reserved. The count is meant to mirror what Google could bill, and an offline laptop used to spend the whole day's allowance on calls that never left it.
func TestWithDailyQuota_RefundsACallThatNeverReachedGoogle(t *testing.T) {
	local := []error{
		fmt.Errorf("no GEMINI_API_KEY: %w", ErrLocalFailure),
		fmt.Errorf("generate: %w", &net.OpError{Op: "dial", Err: errors.New("no route to host")}),
		context.Canceled,
	}
	for _, want := range local {
		state := NewQuotaState(t.TempDir())
		opts := QuotaOptions{"gemini-3.5-flash": {Limit: 1, Reserved: 0}}
		failing := func(context.Context, string) (string, error) { return "", want }
		b := WithDailyQuota(state, "gemini-3.5-flash", true, opts, failing)
		for i := 0; i < 3; i++ {
			var q *ErrDailyQuota
			if _, err := b(context.Background(), "q"); errors.As(err, &q) {
				t.Fatalf("%v: call %d was refused, so the refund did not happen", want, i)
			}
		}
	}
}

// TestWithDailyQuota_CountsACallGoogleAnswered checks the other half: a refusal that came back from Google, and an answer with no text in it, both spent a request against the free tier and must still be counted.
func TestWithDailyQuota_CountsACallGoogleAnswered(t *testing.T) {
	billed := []error{
		nil,
		fmt.Errorf("generate: %w", genai.APIError{Code: 400, Message: "bad request"}),
		errors.New("gemini returned no text"),
	}
	for _, ret := range billed {
		state := NewQuotaState(t.TempDir())
		opts := QuotaOptions{"gemini-3.5-flash": {Limit: 1, Reserved: 0}}
		b := WithDailyQuota(state, "gemini-3.5-flash", true, opts, func(context.Context, string) (string, error) { return "", ret })
		if _, err := b(context.Background(), "q"); ret != nil && !errors.Is(err, ret) {
			t.Fatalf("first call returned %v, want %v", err, ret)
		}
		var q *ErrDailyQuota
		if _, err := b(context.Background(), "q"); !errors.As(err, &q) {
			t.Fatalf("%v: the second call was allowed, so the first was refunded although Google answered it", ret)
		}
	}
}

// TestQuotaOptions_For checks that a Gemini model nobody has measured is still metered, against DefaultQuotaLimit and marked as a guess, while a name from another provider's namespace gets no ceiling at all. Leaving an unlisted Gemini model unmetered meant config.TextFallbackModel and anything pinned in background_models could spend the free tier without a cap.
func TestQuotaOptions_For(t *testing.T) {
	opts := DefaultQuotaOptions()
	if limit, known := opts.For("gemini-3.5-flash"); !known || limit.Limit != 20 {
		t.Errorf("a listed model = (%+v, %v), want its own measured ceiling", limit, known)
	}
	limit, known := opts.For(config.TextFallbackModel)
	if known {
		t.Errorf("%s is reported as measured, but nothing has measured it", config.TextFallbackModel)
	}
	if limit != DefaultQuotaLimit {
		t.Errorf("unlisted gemini model = %+v, want the default ceiling %+v", limit, DefaultQuotaLimit)
	}
	if limit, known := opts.For("sonnet"); known || limit.Limit != 0 {
		t.Errorf("a non-Gemini model name = (%+v, %v), want no ceiling at all", limit, known)
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
