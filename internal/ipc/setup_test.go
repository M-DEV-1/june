package ipc

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"june/internal/config"
)

// TestSetupViewCarriesTheKeyRule checks GET /setup hands the window the same pattern the save route applies, so the window never holds a copy of the rule that can drift.
func TestSetupViewCarriesTheKeyRule(t *testing.T) {
	t.Setenv("JUNE_DATA_DIR", t.TempDir())
	// An empty home and PATH keep the brain rows from reading this machine's logins or running its programs.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	live := NewLiveConfig(&config.JuneConfig{}, nil, func(config.JuneConfig) error { return nil })
	handler := SetupRoutes(SetupDeps{Config: live, DataDir: t.TempDir()})["GET /setup"]

	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/setup", nil))
	var view SetupView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.KeyPattern != plausibleKey.String() {
		t.Errorf("key_pattern = %q, want %q", view.KeyPattern, plausibleKey.String())
	}
}
