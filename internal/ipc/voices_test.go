package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ora/internal/config"
)

// saved collects what a handler persisted, standing in for writing ora-config.json.
func savedConfig(t *testing.T) (*LiveConfig, *config.OraConfig) {
	t.Helper()
	cfg := &config.OraConfig{}
	return NewLiveConfig(cfg, func(config.OraConfig) error { return nil }), cfg
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

// With no voice ever chosen the list still marks one, or the picker opens with nothing selected while the session is in fact speaking as config.DefaultVoice.
func TestVoices_MarksTheDefaultWhenNothingIsChosen(t *testing.T) {
	live, _ := savedConfig(t)
	rec := httptest.NewRecorder()
	Voices(live, nil)(rec, httptest.NewRequest(http.MethodGet, "/voices", nil))
	var out struct{ Voices []VoiceView }
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	for _, v := range out.Voices {
		if v.Current && v.Name != config.DefaultVoice {
			t.Errorf("current voice = %q, want the default %q", v.Name, config.DefaultVoice)
		}
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

// A name Gemini has no voice for is refused and changes nothing: stored, it would be sent on the next connect and fail the session rather than this request.
func TestVoices_PostRefusesAnUnknownName(t *testing.T) {
	live, cfg := savedConfig(t)
	cfg.Voice = "Iapetus"
	rec := httptest.NewRecorder()
	Voices(live, nil)(rec, httptest.NewRequest(http.MethodPost, "/voices", strings.NewReader(`{"name":"Gandalf"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("POST /voices with an unknown name = %d, want 400", rec.Code)
	}
	if cfg.Voice != "Iapetus" {
		t.Errorf("the refused name changed the config to %q", cfg.Voice)
	}
}

// POST /voices/preview speaks a voice without adopting it, which is the whole point of hearing one first.
func TestVoicePreview_SpeaksWithoutChangingTheChoice(t *testing.T) {
	live, cfg := savedConfig(t)
	cfg.Voice = "Iapetus"
	var heard string
	preview := func(ctx context.Context, name string) error { heard = name; return nil }

	rec := httptest.NewRecorder()
	VoicePreview(live, preview)(rec, httptest.NewRequest(http.MethodPost, "/voices/preview", strings.NewReader(`{"name":"puck"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /voices/preview = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if heard != "Puck" {
		t.Errorf("previewed %q, want the canonical Puck", heard)
	}
	if cfg.Voice != "Iapetus" {
		t.Errorf("previewing changed the configured voice to %q", cfg.Voice)
	}
}

// A preview that cannot be spoken says so, rather than answering 200 and leaving the user waiting for a sound that is never coming.
func TestVoicePreview_ReportsAFailureToSpeak(t *testing.T) {
	live, _ := savedConfig(t)
	preview := func(ctx context.Context, name string) error { return errors.New("no speaker") }
	rec := httptest.NewRecorder()
	VoicePreview(live, preview)(rec, httptest.NewRequest(http.MethodPost, "/voices/preview", strings.NewReader(`{"name":"Puck"}`)))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("a failed preview = %d, want 500", rec.Code)
	}
}

// A daemon built without a previewer — no speaker on this machine — says the preview is unavailable instead of panicking on a nil call.
func TestVoicePreview_SaysWhenThereIsNoPreviewer(t *testing.T) {
	live, _ := savedConfig(t)
	rec := httptest.NewRecorder()
	VoicePreview(live, nil)(rec, httptest.NewRequest(http.MethodPost, "/voices/preview", strings.NewReader(`{"name":"Puck"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("preview with no previewer = %d, want 503", rec.Code)
	}
}

// GET /voices carries the two Live models beside the voices, because the picker draws both and one read has to answer both.
func TestVoices_ListsTheLiveModelsAndMarksTheCurrentOne(t *testing.T) {
	live, cfg := savedConfig(t)
	cfg.LiveModel = config.Live25Model

	rec := httptest.NewRecorder()
	Voices(live, nil)(rec, httptest.NewRequest(http.MethodGet, "/voices", nil))
	var out struct{ Models []LiveModelView }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Models) != len(config.LiveVoiceModels) {
		t.Fatalf("models = %d, want the %d Ora can dial", len(out.Models), len(config.LiveVoiceModels))
	}
	var current int
	for _, m := range out.Models {
		if m.Label == "" || m.Trait == "" {
			t.Errorf("model %q has no label or trait", m.Name)
		}
		if m.Current {
			current++
			if m.Name != config.Live25Model {
				t.Errorf("current model = %q, want the configured 2.5", m.Name)
			}
		}
	}
	if current != 1 {
		t.Errorf("%d models marked current, want exactly one", current)
	}
}

// With no model ever chosen the list still marks one, since a session with an empty config dials config.DefaultVoiceModel.
func TestVoices_MarksTheDefaultModelWhenNothingIsChosen(t *testing.T) {
	live, _ := savedConfig(t)
	rec := httptest.NewRecorder()
	Voices(live, nil)(rec, httptest.NewRequest(http.MethodGet, "/voices", nil))
	var out struct{ Models []LiveModelView }
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	for _, m := range out.Models {
		if m.Current && m.Name != config.DefaultVoiceModel {
			t.Errorf("current model = %q, want the default %q", m.Name, config.DefaultVoiceModel)
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

// A model Ora cannot dial is refused and changes nothing: stored, every later session would fail at connect rather than this request failing here.
func TestVoices_PostRefusesAnUnknownModel(t *testing.T) {
	live, cfg := savedConfig(t)
	cfg.LiveModel = config.Live25Model
	rec := httptest.NewRecorder()
	Voices(live, nil)(rec, httptest.NewRequest(http.MethodPost, "/voices", strings.NewReader(`{"model":"gemini-9-ultra"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("POST /voices with an unknown model = %d, want 400", rec.Code)
	}
	if cfg.LiveModel != config.Live25Model {
		t.Errorf("the refused model changed the config to %q", cfg.LiveModel)
	}
}
