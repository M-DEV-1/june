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

	"june/internal/brain"
	"june/internal/config"
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

	cfg := config.JuneConfig{Brain: config.BrainConfig{Provider: config.BrainClaudeCLI, Model: "sonnet"}}
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
	if got := byID["claude"].Models; len(got) != 3 || got[0] != "haiku" || got[1] != "sonnet" || got[2] != "opus" {
		t.Errorf("claude models = %v, want haiku, sonnet and opus", got)
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

// TestBrainsPostUnknownID checks POST /brains rejects a brain id that is not one of the five June knows, and never touches the config.
func TestBrainsPostUnknownID(t *testing.T) {
	t.Setenv("JUNE_DATA_DIR", t.TempDir())
	cfg := &config.JuneConfig{}
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

// TestBrainsPostPersists checks a valid {"brain","model"} pair comes back marked Default with its Model on the same response, is written to the on-disk config, and is read back correctly by a fresh LoadConfig — the point of the route being that the choice survives a daemon restart.
func TestBrainsPostPersists(t *testing.T) {
	t.Setenv("JUNE_DATA_DIR", t.TempDir())
	cfg := &config.JuneConfig{}
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

// The model chip in Settings posts the brain it belongs to with the model picked, and POST /brains made that brain the default as well, so choosing a Codex model moved the user's default brain to Codex. A body with "default": false stores the model and leaves the default brain where it was; the header's brain picker sends no such field and still picks the default.
func TestBrainsPostAModelAloneLeavesTheDefaultBrain(t *testing.T) {
	t.Setenv("JUNE_DATA_DIR", t.TempDir())
	cfg := &config.JuneConfig{Brain: config.BrainConfig{Provider: config.BrainClaudeCLI, Model: "sonnet"}}
	h := Brains(NewLiveConfig(cfg, config.SaveConfig), nil)
	post := func(body string) config.JuneConfig {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/brains", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("POST /brains %s = %d, want 200: %s", body, rec.Code, rec.Body.String())
		}
		return config.LoadConfig()
	}

	reloaded := post(`{"brain":"codex","model":"gpt-5.6-luna","default":false}`)
	if reloaded.Brain.Provider != config.BrainClaudeCLI || reloaded.Brain.Model != "sonnet" {
		t.Errorf("brain on disk = %+v, want the default left on %q with sonnet", reloaded.Brain, config.BrainClaudeCLI)
	}
	if reloaded.BrainModels["codex"] != "gpt-5.6-luna" {
		t.Errorf("brain_models[codex] on disk = %q, want gpt-5.6-luna", reloaded.BrainModels["codex"])
	}
	// A model picked for the default brain itself is the model that brain is called with.
	if reloaded = post(`{"brain":"claude","model":"opus","default":false}`); reloaded.Brain.Model != "opus" {
		t.Errorf("the default brain's model on disk = %q, want opus", reloaded.Brain.Model)
	}
}

// TestBrainsPostCodexPersistsItsOwnProvider checks POST /brains {"brain":"codex"} persists config.BrainCodex and comes back with the codex row marked Default, not the gemini row — the bug this covers stored config.BrainGeminiAPI for codex, which made "codex" indistinguishable from "gemini" on disk and on the very next GET.
func TestBrainsPostCodexPersistsItsOwnProvider(t *testing.T) {
	t.Setenv("JUNE_DATA_DIR", t.TempDir())
	cfg := &config.JuneConfig{}
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
	Brains(NewLiveConfig(&config.JuneConfig{}, config.SaveConfig), limitsFor)(rec, httptest.NewRequest(http.MethodGet, "/brains", nil))
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
	if len(out.Brains) != len(brainIDs) || byID["claude"].Note == "" || byID["ollama"].Name == "" {
		t.Errorf("a row lost a field: %+v", out.Brains)
	}
}

// TestBrains_ConcurrentPostAndRead runs POST /brains against GET /brains under the race detector, which is what a brain pick while the picker refetches looks like; the GET reads the BrainModels map, so a shared map would be caught here.
func TestBrains_ConcurrentPostAndRead(t *testing.T) {
	live := NewLiveConfig(&config.JuneConfig{}, func(config.JuneConfig) error { return nil })
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
	list := brainList(context.Background(), config.JuneConfig{}, t.TempDir(), func(name string) bool { return true }, nil)
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
	// Antigravity rather than Gemini: the gemini row reads the metered API key out of the environment, where every other row reads a binary or a login file off the machine.
	if !byID["grok"].SignedIn || !byID["antigravity"].SignedIn {
		t.Errorf("a brain with a working backend was marked unavailable: %+v", list)
	}
}

// TestBrainsPostRefusesABrainWithNoBackend checks a pick of a brain internal/brain cannot answer with is refused with the reason, rather than persisted. GET already marks the Ollama row unavailable, but the POST accepted it: the config then held ollama-cli and FromConfig failed every call to it with ErrNoBackend, so every duty pinned to that brain handed on to a provider the user did not choose.
func TestBrainsPostRefusesABrainWithNoBackend(t *testing.T) {
	t.Setenv("JUNE_DATA_DIR", t.TempDir())
	cfg := &config.JuneConfig{}
	rec := httptest.NewRecorder()
	Brains(NewLiveConfig(cfg, config.SaveConfig), nil)(rec, httptest.NewRequest(http.MethodPost, "/brains", strings.NewReader(`{"brain":"ollama","model":"llama3"}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /brains with ollama = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), brain.NoBackendNote(config.BrainOllama)) {
		t.Errorf("the refusal says %q, want it to carry the reason the row is unavailable", rec.Body.String())
	}
	if cfg.Brain.Provider != "" {
		t.Errorf("the refused pick changed the provider to %q", cfg.Brain.Provider)
	}
	if _, err := os.Stat(config.ConfigPath()); err == nil {
		t.Errorf("the refused pick wrote a config file")
	}
}

// `agy models` prints two columns and no header, so every non-blank line's first field is a model id. `ollama list` prints a header first. One parser reads both, and getting the header wrong would either drop a real model or offer "NAME" as one.
func TestFirstFields_ReadsBothTableShapes(t *testing.T) {
	agy := "gemini-3.8-flash-high     Gemini 3.8 Flash (High)\nclaude-sonnet-4-6         Claude Sonnet 4.6 (Thinking)\ngpt-oss-120b-medium       GPT-OSS 120B (Medium)\n"
	got := firstFields(agy, false)
	want := []string{"gemini-3.8-flash-high", "claude-sonnet-4-6", "gpt-oss-120b-medium"}
	if len(got) != len(want) {
		t.Fatalf("agy models = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("agy model %d = %q, want %q", i, got[i], want[i])
		}
	}

	ollama := "NAME              ID    SIZE\nembeddinggemma    abc   600 MB\n"
	if got := firstFields(ollama, true); len(got) != 1 || got[0] != "embeddinggemma" {
		t.Errorf("ollama list = %v, want just the one model with the header skipped", got)
	}
}

// The Antigravity row reported signed in for as long as the agy binary was on PATH, which says nothing about whether its login still works: on 2026-09-15 the picker drew it as available while every ask to it failed the eligibility check with a 401. `agy models` fails with that same UNAUTHENTICATED check, so the roster June already reads for the picker is the health check — it costs no model tokens and is already cached.
// Only that refusal is evidence of a dead login. A read that fails for any other reason — offline, the thirty-second timeout, a crash — also came back as an empty roster, and the row was marked signed out on it for the cache's ten minutes.
func TestBrains_AntigravityIsSignedOutOnlyWhenItsRosterReadIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		signedIn     bool
	}{
		{"the login is refused", `echo 'Eligibility check failed: UNAUTHENTICATED (code 401): Request had invalid authentication credentials.' >&2; exit 1`, false},
		{"the machine is offline", `echo 'dial tcp: lookup the backend: no such host' >&2; exit 1`, true},
		{"the roster reads", `echo 'gemini-3.8-flash-high     Gemini 3.8 Flash (High)'`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "agy"), []byte("#!/bin/sh\n"+tc.script+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			setAgyRoster(t, nil, false)
			agyCache.refresh(agyModels)

			row := brainRow(t, brainList(context.Background(), config.JuneConfig{}, t.TempDir(), func(string) bool { return true }, nil), "antigravity")
			if row.SignedIn != tc.signedIn {
				t.Errorf("signed in = %v, want %v", row.SignedIn, tc.signedIn)
			}
			if !tc.signedIn && row.LimitsNote == "" {
				t.Error("limits_note is empty, want the sentence saying to sign in again")
			}
		})
	}
}

func TestBrains_AntigravityStaysSignedInBeforeTheFirstRosterRead(t *testing.T) {
	setAgyRoster(t, nil, false)
	list := brainList(context.Background(), config.JuneConfig{}, t.TempDir(), func(string) bool { return true }, nil)
	if !brainRow(t, list, "antigravity").SignedIn {
		t.Error("antigravity was marked signed out on a roster nobody has read yet")
	}
}

// setAgyRoster puts a known roster in the package cache for one test. read is whether it should look like a read that has finished, which is what makes an empty roster mean "signed out" rather than "not looked yet". The fields are set under the lock rather than assigned as a struct, because the cache holds a mutex.
func setAgyRoster(t *testing.T, models []string, read bool) {
	t.Helper()
	set := func(models []string, read bool) {
		agyCache.mu.Lock()
		defer agyCache.mu.Unlock()
		agyCache.models = models
		if read {
			agyCache.readAt = time.Now()
		} else {
			agyCache.readAt = time.Time{}
		}
		agyCache.reading = !read
		agyRefused.Store(false)
	}
	set(models, read)
	t.Cleanup(func() { set(nil, false) })
}

func brainRow(t *testing.T, list []BrainView, id string) BrainView {
	t.Helper()
	for _, b := range list {
		if b.ID == id {
			return b
		}
	}
	t.Fatalf("no %s row in %+v", id, list)
	return BrainView{}
}

// A login that has stopped working shows on the row before anything is asked of it. On 2026-09-15 the Antigravity credential was refused for hours while the picker drew the row as available, and the first anyone knew of it was three questions dying with an empty error; Claude was signed in the whole time and never asked.
// The signal is the usage reading each brain already fetches when the picker renders, because that needs a working credential and is made whether or not anyone asks a question. A reader that is refused records SignedOut with the reason, and that is what the row draws.
func TestBrains_ASignedOutLoginShowsOnTheRowBeforeAnythingIsAsked(t *testing.T) {
	setAgyRoster(t, nil, false)
	refused := func(ctx context.Context, id string) (brain.UsageSnapshot, bool) {
		if id != "grok" {
			return brain.UsageSnapshot{}, false
		}
		return brain.UsageSnapshot{SignedOut: true, Note: "the grok login was refused: sign in again with grok", At: time.Now()}, true
	}
	list := brainList(context.Background(), config.JuneConfig{}, t.TempDir(), func(string) bool { return true }, refused)
	row := brainRow(t, list, "grok")
	if row.SignedIn {
		t.Error("grok reports signed in although its usage reading was refused")
	}
	if row.LimitsNote == "" {
		t.Error("grok says nothing about why it cannot be picked")
	}
	// A brain whose reading was never taken is untouched: absence of a reading is not evidence of a dead login.
	if !brainRow(t, list, "antigravity").SignedIn {
		t.Error("antigravity was marked signed out on the strength of another brain's refusal")
	}
}
