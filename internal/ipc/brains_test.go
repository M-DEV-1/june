package ipc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ora/internal/brain"
	"ora/internal/config"
)

// TestBrainsReadsTheLoginFiles checks the two signals a brain's row rests on: a login file on disk, and a binary on PATH. The Claude credentials file here carries a token as well as the plan name, so this also checks the token never reaches the response.
func TestBrainsReadsTheLoginFiles(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"sk-do-not-leak","subscriptionType":"max"}}`), 0600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}

	cfg := config.OraConfig{Brain: config.BrainConfig{Provider: config.BrainClaudeCLI, Model: "sonnet"}}
	onPath := func(name string) bool { return name == "grok" }

	list := brainList(context.Background(), cfg, home, onPath, nil)
	byID := map[string]BrainView{}
	for _, b := range list {
		byID[b.ID] = b
	}
	for _, id := range []string{"claude", "codex", "gemini", "grok", "ollama"} {
		if _, ok := byID[id]; !ok {
			t.Fatalf("brains = %+v, missing %q", list, id)
		}
	}

	if !byID["claude"].SignedIn {
		t.Errorf("claude is not signed in although its credentials file exists")
	}
	if byID["claude"].Account != "max" {
		t.Errorf("claude account = %q, want the plan named in the file", byID["claude"].Account)
	}
	if !byID["claude"].Default {
		t.Errorf("claude is the configured brain but is not marked default")
	}
	if got := byID["claude"].Models; len(got) != 2 || got[0] != "sonnet" || got[1] != "opus" {
		t.Errorf("claude models = %v, want sonnet and opus", got)
	}
	if byID["claude"].Model != "sonnet" {
		t.Errorf("claude model = %q, want the config's sonnet since it is the default brain and no per-brain choice was ever posted", byID["claude"].Model)
	}
	if byID["codex"].SignedIn {
		t.Errorf("codex reports signed in with no auth.json on disk")
	}
	if !byID["grok"].SignedIn {
		t.Errorf("grok is not signed in although its binary is on PATH")
	}
	if byID["grok"].Models == nil || len(byID["grok"].Models) != 0 {
		t.Errorf("grok models = %v, want an empty list — that CLI exposes no model choice", byID["grok"].Models)
	}
	if byID["grok"].LimitsNote == "" {
		t.Errorf("grok limits_note is empty, want a sentence saying the CLI exposes no usage data")
	}
	if byID["gemini"].SignedIn {
		t.Errorf("gemini reports signed in with no agy binary on PATH")
	}
	if len(byID["gemini"].Models) == 0 {
		t.Errorf("gemini has no models, want the configured one")
	}
	for _, b := range list {
		if b.Note == "" {
			t.Errorf("brain %q has no note", b.ID)
		}
		if b.Name == "" {
			t.Errorf("brain %q has no name", b.ID)
		}
	}

	body, err := json.Marshal(map[string]any{"brains": list})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "sk-do-not-leak") {
		t.Fatalf("a token from the credentials file reached the response")
	}
}

// TestBrainsHandlerShape checks the route itself answers with the list the window reads, never null.
func TestBrainsHandlerShape(t *testing.T) {
	rec := httptest.NewRecorder()
	Brains(NewLiveConfig(&config.OraConfig{}, config.SaveConfig), nil)(rec, httptest.NewRequest(http.MethodGet, "/brains", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /brains = %d, want 200", rec.Code)
	}
	var out struct{ Brains []BrainView }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Brains) != 5 {
		t.Errorf("brains = %d, want the five Ora knows about", len(out.Brains))
	}
	var defaults int
	for _, b := range out.Brains {
		if b.Default {
			defaults++
		}
	}
	if defaults != 1 {
		t.Errorf("%d brains marked default, want exactly one", defaults)
	}
}

// TestBrainsPostUnknownID checks POST /brains rejects a brain id that is not one of the five Ora knows, and never touches the config.
func TestBrainsPostUnknownID(t *testing.T) {
	t.Setenv("ORA_DATA_DIR", t.TempDir())
	cfg := &config.OraConfig{}
	rec := httptest.NewRecorder()
	Brains(NewLiveConfig(cfg, config.SaveConfig), nil)(rec, httptest.NewRequest(http.MethodPost, "/brains", strings.NewReader(`{"brain":"chatgpt","model":"whatever"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /brains with an unknown id = %d, want 400", rec.Code)
	}
	if cfg.Brain.Provider != "" {
		t.Errorf("an unknown id changed the provider to %q", cfg.Brain.Provider)
	}
	if _, err := os.Stat(config.ConfigPath()); err == nil {
		t.Errorf("an unknown id wrote a config file")
	}
}

// TestProviderForBrainID checks every brain id the window can post maps to its own distinct, non-empty provider constant, so POST /brains never persists one brain's choice under another brain's name — the bug this covers had "codex" fall through to config.BrainGeminiAPI, which made the codex row disappear behind the Gemini one instead of ever showing Default.
func TestProviderForBrainID(t *testing.T) {
	ids := []string{"claude", "codex", "gemini", "grok", "ollama"}
	seen := map[string]string{}
	for _, id := range ids {
		provider, ok := providerForBrainID(id)
		if !ok {
			t.Fatalf("providerForBrainID(%q) reported not ok, want one of the five known brains", id)
		}
		if provider == "" {
			t.Errorf("providerForBrainID(%q) = \"\", want a non-empty provider", id)
		}
		if other, dup := seen[provider]; dup {
			t.Errorf("providerForBrainID(%q) = %q, the same provider already given to %q; every brain needs its own", id, provider, other)
		}
		seen[provider] = id
	}
	if got, _ := providerForBrainID("codex"); got != config.BrainCodex {
		t.Errorf(`providerForBrainID("codex") = %q, want %q (not the Gemini provider)`, got, config.BrainCodex)
	}
	if got, _ := providerForBrainID("codex"); got == config.BrainGeminiAPI {
		t.Errorf(`providerForBrainID("codex") = %q, must not be the Gemini provider`, got)
	}
}

// TestBrainsPostPersists checks a valid {"brain","model"} pair comes back marked Default with its Model on the same response, is written to the on-disk config, and is read back correctly by a fresh LoadConfig — the point of the route being that the choice survives a daemon restart.
func TestBrainsPostPersists(t *testing.T) {
	t.Setenv("ORA_DATA_DIR", t.TempDir())
	cfg := &config.OraConfig{}
	rec := httptest.NewRecorder()
	Brains(NewLiveConfig(cfg, config.SaveConfig), nil)(rec, httptest.NewRequest(http.MethodPost, "/brains", strings.NewReader(`{"brain":"claude","model":"opus"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /brains = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var out struct{ Brains []BrainView }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byID := map[string]BrainView{}
	for _, b := range out.Brains {
		byID[b.ID] = b
	}
	if !byID["claude"].Default {
		t.Errorf("claude was just picked but is not marked default")
	}
	if byID["claude"].Model != "opus" {
		t.Errorf("claude model in the response = %q, want opus", byID["claude"].Model)
	}

	reloaded := config.LoadConfig()
	if reloaded.Brain.Provider != config.BrainClaudeCLI {
		t.Errorf("provider on disk = %q, want %q", reloaded.Brain.Provider, config.BrainClaudeCLI)
	}
	if reloaded.BrainModels["claude"] != "opus" {
		t.Errorf("brain_models[claude] on disk = %q, want opus", reloaded.BrainModels["claude"])
	}
}

// TestBrainsPostCodexPersistsItsOwnProvider checks POST /brains {"brain":"codex"} persists config.BrainCodex and comes back with the codex row marked Default, not the gemini row — the bug this covers stored config.BrainGeminiAPI for codex, which made "codex" indistinguishable from "gemini" on disk and on the very next GET.
func TestBrainsPostCodexPersistsItsOwnProvider(t *testing.T) {
	t.Setenv("ORA_DATA_DIR", t.TempDir())
	cfg := &config.OraConfig{}
	rec := httptest.NewRecorder()
	Brains(NewLiveConfig(cfg, config.SaveConfig), nil)(rec, httptest.NewRequest(http.MethodPost, "/brains", strings.NewReader(`{"brain":"codex","model":"gpt-5.5"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /brains = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var out struct{ Brains []BrainView }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byID := map[string]BrainView{}
	for _, b := range out.Brains {
		byID[b.ID] = b
	}
	if !byID["codex"].Default {
		t.Errorf("codex was just picked but is not marked default")
	}
	if byID["gemini"].Default {
		t.Errorf("gemini is marked default after codex was picked")
	}
	if got := byID["gemini"].Models; len(got) != 0 && got[0] == "gpt-5.5" {
		t.Errorf("gemini models = %v, a codex model must not leak into gemini's own list", got)
	}

	reloaded := config.LoadConfig()
	if reloaded.Brain.Provider != config.BrainCodex {
		t.Errorf("provider on disk = %q, want %q", reloaded.Brain.Provider, config.BrainCodex)
	}
	if reloaded.Brain.Provider == config.BrainGeminiAPI {
		t.Errorf("provider on disk is the Gemini provider, codex must persist its own")
	}
}

// TestBrainsCarriesTheUsageBars checks each brain row carries whatever allowance windows its provider exposes, with the moment they were read, and an empty array — never null — for a brain that exposes none. This is what the picker draws its mini bars from.
func TestBrainsCarriesTheUsageBars(t *testing.T) {
	read := time.Date(2026, 9, 5, 14, 0, 0, 0, time.UTC)
	limitsFor := func(_ context.Context, id string) (brain.UsageSnapshot, bool) {
		switch id {
		case "codex":
			return brain.UsageSnapshot{At: read, Limits: []brain.UsageLimit{
				{Window: "5h", UsedFraction: 0.65, ResetsAt: read.Add(3*time.Hour + 56*time.Minute), Source: "x-codex-primary-*"},
				{Window: "weekly", UsedFraction: 0.37, ResetsAt: read.Add(76 * time.Hour), Source: "x-codex-secondary-*"},
			}}, true
		case "gemini":
			return brain.UsageSnapshot{At: read, Limits: []brain.UsageLimit{
				{Window: "daily", UsedFraction: 0.25, ResetsAt: read.Add(10 * time.Hour), Source: "brain_quota.json gemini-3.5-flash"},
			}}, true
		}
		return brain.UsageSnapshot{}, false
	}

	rec := httptest.NewRecorder()
	Brains(NewLiveConfig(&config.OraConfig{}, config.SaveConfig), limitsFor)(rec, httptest.NewRequest(http.MethodGet, "/brains", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /brains = %d, want 200", rec.Code)
	}
	var out struct{ Brains []BrainView }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byID := map[string]BrainView{}
	for _, b := range out.Brains {
		byID[b.ID] = b
	}

	codex := byID["codex"]
	if len(codex.Limits) != 2 {
		t.Fatalf("codex limits = %+v, want the five-hour and weekly windows", codex.Limits)
	}
	if codex.Limits[0].Window != "5h" || codex.Limits[0].UsedFraction != 0.65 {
		t.Errorf("codex first limit = %+v, want 5h at 0.65", codex.Limits[0])
	}
	if codex.LimitsAt != read.Format(time.RFC3339) {
		t.Errorf("codex limits_at = %q, want %q", codex.LimitsAt, read.Format(time.RFC3339))
	}
	if byID["gemini"].Limits[0].Window != "daily" {
		t.Errorf("gemini limits = %+v, want the daily request ceiling", byID["gemini"].Limits)
	}
	for _, id := range []string{"grok", "ollama", "claude"} {
		row := byID[id]
		if row.Limits == nil {
			t.Errorf("%s limits are null, want an empty array", id)
		}
		if len(row.Limits) != 0 || row.LimitsAt != "" {
			t.Errorf("%s reports limits %+v at %q, want none: nothing exposes an allowance for it here", id, row.Limits, row.LimitsAt)
		}
	}

	// The existing rows must be untouched by the new ones.
	if len(out.Brains) != 5 || byID["claude"].Note == "" || byID["ollama"].Name == "" {
		t.Errorf("the five rows lost a field: %+v", out.Brains)
	}
}

// TestBrainsClaudeUsageNoteWhenTurnedOff checks the claude row explains an empty usage bar when the user turned the login-based fetch off in Settings, the same way GrokNote explains Grok's — rather than looking like nothing has been read yet.
func TestBrainsClaudeUsageNoteWhenTurnedOff(t *testing.T) {
	off := false
	cfg := config.OraConfig{ClaudeUsageFromLogin: &off}

	list := brainList(context.Background(), cfg, t.TempDir(), func(string) bool { return false }, nil)
	byID := map[string]BrainView{}
	for _, b := range list {
		byID[b.ID] = b
	}

	if got := byID["claude"].LimitsNote; got != "turned off in Settings" {
		t.Errorf("claude limits_note = %q, want the reason it was turned off", got)
	}
	if len(byID["claude"].Limits) != 0 {
		t.Errorf("claude limits = %+v, want none while turned off", byID["claude"].Limits)
	}
}

// TestBrainsClaudeUsageNoteAbsentWhenOn checks the default config — the setting unset — carries no note, so an ordinary machine's row reads as "nothing read yet" rather than "turned off".
func TestBrainsClaudeUsageNoteAbsentWhenOn(t *testing.T) {
	list := brainList(context.Background(), config.OraConfig{}, t.TempDir(), func(string) bool { return false }, nil)
	for _, b := range list {
		if b.ID == "claude" && b.LimitsNote != "" {
			t.Errorf("claude limits_note = %q, want empty when the setting is on", b.LimitsNote)
		}
	}
}

// TestBrains_ConcurrentPostAndRead runs POST /brains against GET /brains under the race detector, which is what a brain pick while the picker refetches looks like; the GET reads the BrainModels map, so a shared map would be caught here.
func TestBrains_ConcurrentPostAndRead(t *testing.T) {
	live := NewLiveConfig(&config.OraConfig{}, func(config.OraConfig) error { return nil })
	h := Brains(live, nil)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			h(rec, httptest.NewRequest(http.MethodPost, "/brains", strings.NewReader(`{"brain":"claude","model":"opus"}`)))
		}()
		go func() {
			defer wg.Done()
			// A real GET, which reads the BrainModels map five times after the lock is released, is what a picker refetch does while a pick is being written.
			rec := httptest.NewRecorder()
			h(rec, httptest.NewRequest(http.MethodGet, "/brains", nil))
		}()
	}
	wg.Wait()
	if got := live.Get().Brain.Model; got != "opus" {
		t.Errorf("model after the posts = %q, want opus", got)
	}
}

// TestBrains_OllamaIsUnavailableWithTheReason checks the Ollama row is never offered as pickable, even with the binary installed and models listed, and says why. internal/brain has no Ollama backend, so picking it used to persist provider "ollama" and then answer every duty on the Gemini API instead — spending the metered free tier the user picked Ollama to avoid and sending the prompt to Google.
// The window's picker disables a row with signed_in false and prints "Not signed in" in place of the bar (app/src/next/parts.tsx), so the note is what the settings page and a reader of the JSON get; the row stays in the list, and its name, note and models are untouched.
func TestBrains_OllamaIsUnavailableWithTheReason(t *testing.T) {
	list := brainList(context.Background(), config.OraConfig{}, t.TempDir(), func(name string) bool { return true }, nil)
	byID := map[string]BrainView{}
	for _, b := range list {
		byID[b.ID] = b
	}
	if byID["ollama"].SignedIn {
		t.Error("ollama reports signed in, but nothing in the daemon can answer with it")
	}
	if byID["ollama"].LimitsNote == "" {
		t.Error("ollama limits_note is empty, want the sentence saying why the row cannot be picked")
	}
	if byID["ollama"].Name == "" || byID["ollama"].Note == "" {
		t.Errorf("the ollama row lost a field: %+v", byID["ollama"])
	}
	// Every other brain still reads its own signal off the machine.
	if !byID["grok"].SignedIn || !byID["gemini"].SignedIn {
		t.Errorf("a brain with a working backend was marked unavailable: %+v", list)
	}
}

// TestBrains_LimitsNoteCarriesTheSnapshotsOwnNote checks that a caveat on a reading — today, that a Gemini model's ceiling is a default because nobody has measured that model — reaches the row the picker draws, rather than being dropped between brain.GeminiDaily and the JSON.
func TestBrains_LimitsNoteCarriesTheSnapshotsOwnNote(t *testing.T) {
	limits := func(ctx context.Context, id string) (brain.UsageSnapshot, bool) {
		if id != "gemini" {
			return brain.UsageSnapshot{}, false
		}
		return brain.UsageSnapshot{
			Limits: []brain.UsageLimit{{Window: "daily", UsedFraction: 0.1}},
			At:     time.Now(),
			Note:   "the ceiling is a default",
		}, true
	}
	list := brainList(context.Background(), config.OraConfig{}, t.TempDir(), func(string) bool { return false }, limits)
	for _, b := range list {
		if b.ID != "gemini" {
			continue
		}
		if b.LimitsNote != "the ceiling is a default" {
			t.Errorf("gemini limits_note = %q, want the snapshot's own note", b.LimitsNote)
		}
	}
}
