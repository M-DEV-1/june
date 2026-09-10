package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeBrainModels puts an ora-config.json naming a model for each brain id into a fresh data directory and points the config loader at it.
func writeBrainModels(t *testing.T, models map[string]string) {
	t.Helper()
	dir := t.TempDir()
	body, err := json.Marshal(map[string]any{"brain_models": models})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ora-config.json"), body, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("ORA_DATA_DIR", dir)
}

// The model the user picked in the window is the model the ask runs on. Before this, POST /brains stored the choice and every asker read an environment variable instead, so picking Sonnet in Settings changed the row on screen and nothing else.
func TestPickedModel_UsesTheModelTheWindowStored(t *testing.T) {
	writeBrainModels(t, map[string]string{"claude": "sonnet", "antigravity": "gemini-3-pro"})
	t.Setenv("ORA_CLAUDE_MODEL", "")

	if got := pickedModel("claude", "ORA_CLAUDE_MODEL", "opus"); got != "sonnet" {
		t.Errorf("claude asks for %q, want the stored sonnet", got)
	}
	if got := pickedModel("antigravity", "ORA_AGY_MODEL", ""); got != "gemini-3-pro" {
		t.Errorf("antigravity asks for %q, want the stored gemini-3-pro", got)
	}
}

// With nothing stored for that brain the environment override still works, which is how a developer pins a model without touching the config.
func TestPickedModel_FallsBackToTheEnvironment(t *testing.T) {
	writeBrainModels(t, map[string]string{"claude": "sonnet"})
	t.Setenv("ORA_AGY_MODEL", "gemini-3-flash")

	if got := pickedModel("antigravity", "ORA_AGY_MODEL", ""); got != "gemini-3-flash" {
		t.Errorf("antigravity asks for %q, want the environment's gemini-3-flash", got)
	}
}

// With neither a stored choice nor an override the caller's own default is used, so a config written before BrainModels existed asks for exactly what it asked for before.
func TestPickedModel_FallsBackToTheCallersDefault(t *testing.T) {
	writeBrainModels(t, nil)
	t.Setenv("ORA_CLAUDE_MODEL", "")

	if got := pickedModel("claude", "ORA_CLAUDE_MODEL", "opus"); got != "opus" {
		t.Errorf("claude asks for %q, want the default opus", got)
	}
}
