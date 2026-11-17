package ipc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"june/internal/config"
)

// TestSetupViewWindowFrames checks window_frames follows the daemon's answer while setup is under way, is left out when there is none, and is not asked for once setup is done.
func TestSetupViewWindowFrames(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name      string
		done      bool
		answer    *bool
		wantField bool
		wantValue bool
	}{
		{"not loaded", false, &no, true, false},
		{"loaded", false, &yes, true, true},
		{"nothing to wait for", false, nil, false, false},
		{"setup done", true, &no, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("JUNE_DATA_DIR", t.TempDir())
			// An empty home and PATH keep the brain rows from reading this machine's logins or running its programs.
			t.Setenv("HOME", t.TempDir())
			t.Setenv("PATH", t.TempDir())
			live := NewLiveConfig(&config.JuneConfig{SetupDone: c.done}, nil, func(config.JuneConfig) error { return nil })
			handler := SetupRoutes(SetupDeps{Config: live, DataDir: t.TempDir(), WindowFrames: func(context.Context) *bool { return c.answer }})["GET /setup"]

			rec := httptest.NewRecorder()
			handler(rec, httptest.NewRequest(http.MethodGet, "/setup", nil))
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			got, present := body["window_frames"]
			if present != c.wantField || (present && got != c.wantValue) {
				t.Errorf("window_frames = %v (present %v), want %v (present %v)", got, present, c.wantValue, c.wantField)
			}
		})
	}
}
