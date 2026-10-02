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
