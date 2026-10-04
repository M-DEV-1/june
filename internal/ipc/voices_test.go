package ipc

import (
	"encoding/json"

	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"june/internal/config"
)

// saved collects what a handler persisted, standing in for writing june-config.json.
func savedConfig(t *testing.T) (*LiveConfig, *config.JuneConfig) {
	t.Helper()
	cfg := &config.JuneConfig{}
	return NewLiveConfig(cfg, nil, func(config.JuneConfig) error { return nil }), cfg
}

// GET /voices is the whole roster with the one in use marked, since the picker draws every voice and has to show which is current without a second call.
func TestVoices_ListsEveryVoiceAndMarksTheCurrentOne(t *testing.T) {
	live, cfg := savedConfig(t)
	cfg.Voice = "Sulafat"

	rec := httptest.NewRecorder()
	Voices(live, nil)(rec, httptest.NewRequest(http.MethodGet, "/voices", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /voices = %d, want 200", rec.Code)
	}
	var out struct{ Voices []VoiceView }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Voices) != len(config.AvailableVoices) {
		t.Fatalf("voices = %d, want the %d Gemini exposes", len(out.Voices), len(config.AvailableVoices))
	}
	var current int
	for _, v := range out.Voices {
		if v.Current {
			current++
			if v.Name != "Sulafat" {
				t.Errorf("current voice = %q, want the configured Sulafat", v.Name)
			}
		}
		// The trait is the whole reason to draw a list rather than a plain dropdown: thirty star names say nothing about how they sound.
		if v.Trait == "" {
			t.Errorf("voice %q has no trait", v.Name)
		}
	}
	if current != 1 {
		t.Errorf("%d voices marked current, want exactly one", current)
	}
}

// POST /voices persists the canonical spelling, so a name typed in any case is stored the way Gemini expects to receive it.
func TestVoices_PostPersistsTheCanonicalSpelling(t *testing.T) {
	live, cfg := savedConfig(t)
	rec := httptest.NewRecorder()
	Voices(live, nil)(rec, httptest.NewRequest(http.MethodPost, "/voices", strings.NewReader(`{"name":"kore"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /voices = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if cfg.Voice != "Kore" {
		t.Errorf("stored voice = %q, want the canonical Kore", cfg.Voice)
	}
	var out struct{ Voices []VoiceView }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, v := range out.Voices {
		if v.Current && v.Name != "Kore" {
			t.Errorf("the answer still marks %q current after picking Kore", v.Name)
		}
	}
}

// POST {"model": ...} persists the choice and takes effect in the same process, so the next session dials it without a restart.
func TestVoices_PostSwitchesTheLiveModel(t *testing.T) {
	t.Cleanup(func() { config.SetVoiceModel(config.DefaultVoiceModel) })
	live, cfg := savedConfig(t)
	cfg.Voice = "Sulafat"

	rec := httptest.NewRecorder()
	body := strings.NewReader(`{"model":"` + config.Live25Model + `"}`)
	Voices(live, nil)(rec, httptest.NewRequest(http.MethodPost, "/voices", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /voices = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if cfg.LiveModel != config.Live25Model {
		t.Errorf("stored model = %q, want 2.5", cfg.LiveModel)
	}
	if config.VoiceModel() != config.Live25Model {
		t.Errorf("the running daemon still dials %q", config.VoiceModel())
	}
	if cfg.Voice != "Sulafat" {
		t.Errorf("switching the model changed the voice to %q", cfg.Voice)
	}
}
