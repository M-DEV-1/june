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
