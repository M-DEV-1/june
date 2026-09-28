// voices.go holds GET/POST /voices and POST /voices/preview: which voice June speaks in during a live session, and hearing one before adopting it. The roster is Gemini's own thirty prebuilt voices (config.AvailableVoices); the trait beside each name is what the picker shows, because thirty star names say nothing about how any of them sounds.
package ipc

import (
	"context"
	"net/http"

	"june/internal/config"
	"june/internal/util"
)

// LiveModelView is one of the Live models on GET or POST /voices. Trait is what choosing it costs and buys, in one line, because the trade between them — tone against about four seconds of latency, and whether the model decides for itself that audio was aimed at it — is the whole reason there is a choice.
type LiveModelView struct {
	Name    string `json:"name"`
	Label   string `json:"label"`
	Trait   string `json:"trait"`
	Current bool   `json:"current"`
}

// VoiceView is one voice on GET or POST /voices. Trait is Google's own one-word description of how it sounds ("Bright", "Gravelly"), and Current marks the one a live session will speak in.
type VoiceView struct {
	Name    string `json:"name"`
	Trait   string `json:"trait"`
	Current bool   `json:"current"`
}

// VoicePreviewer speaks one line in a named voice. Input: a context and the canonical voice name. Output: an error when the line could not be synthesised or played. The daemon passes agent.SpeakPreview wired to a speaker; the tests pass a function that only records the name.
type VoicePreviewer func(ctx context.Context, name string) error

// Voices builds the /voices handler. GET answers the whole roster with the configured voice marked. POST {"name": string} takes a voice in any casing, stores its canonical spelling and persists it, then answers the same list GET would; a name Gemini has no voice for is 400 and changes nothing. Input: the config accessor shared with the rest of the daemon, and the previewer, which this handler does not use but takes so both routes are built from one call site. Output: the handler.
// A session already running keeps the voice it connected with — connect.go reads the choice when it dials — so a change here is heard on the next session.
func Voices(cfg *LiveConfig, _ VoicePreviewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeVoices(w, cfg.Get())
		case http.MethodPost:
			var req struct {
				Name  string `json:"name"`
				Model string `json:"model"`
			}
			if !DecodeJSON(w, r, &req) {
				return
			}
			// One route, two choices: which voice speaks, and which Live model it speaks through. A body may carry either, and a body carrying neither is a bad request rather than a silent no-op.
			if req.Model != "" {
				if !config.ValidVoiceModel(req.Model) {
					http.Error(w, "unknown live model: "+req.Model, http.StatusBadRequest)
					return
				}
				if err := cfg.Update(func(c *config.JuneConfig) { c.LiveModel = req.Model }); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				config.SetVoiceModel(req.Model)
				writeVoices(w, cfg.Get())
				return
			}
			canonical, ok := config.NormalizeVoice(req.Name)
			if !ok {
				http.Error(w, "unknown voice: "+req.Name, http.StatusBadRequest)
				return
			}
			if err := cfg.Update(func(c *config.JuneConfig) { c.Voice = canonical }); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeVoices(w, cfg.Get())
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// VoicePreview builds the /voices/preview handler. POST {"name": string} speaks one fixed line in that voice through this machine's speaker and answers 200 once it has played; it never changes which voice is configured, which is the whole point of hearing one first. An unknown name is 400, a daemon with no speaker is 503, and a synthesis or playback failure is 500 — a preview that answers 200 and stays silent leaves the user waiting for a sound that is never coming. Input: the config accessor and the previewer, which may be nil. Output: the handler.
func VoicePreview(cfg *LiveConfig, preview VoicePreviewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Name string `json:"name"`
		}
		if !DecodeJSON(w, r, &req) {
			return
		}
		canonical, ok := config.NormalizeVoice(req.Name)
		if !ok {
			http.Error(w, "unknown voice: "+req.Name, http.StatusBadRequest)
			return
		}
		if preview == nil {
			http.Error(w, "this daemon has no speaker to preview a voice through", http.StatusServiceUnavailable)
			return
		}
		if err := preview(r.Context(), canonical); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		util.WriteJSON(w, map[string]any{"played": canonical})
	}
}

// writeVoices writes the roster for cfg as JSON, the body both GET and POST /voices answer with. The voice marked current is the configured one, or config.DefaultVoice when nothing has ever been chosen — which is the voice a session would actually speak in, so the picker opens on it rather than on nothing.
func writeVoices(w http.ResponseWriter, cfg config.JuneConfig) {
	current, ok := config.NormalizeVoice(cfg.Voice)
	if !ok {
		current = config.DefaultVoice
	}
	list := make([]VoiceView, 0, len(config.AvailableVoices))
	for _, name := range config.AvailableVoices {
		list = append(list, VoiceView{Name: name, Trait: config.VoiceTrait(name), Current: name == current})
	}
	// The model the config names, not the one the daemon is running on: a change takes effect on the next session, and the picker has to show what was chosen rather than what is still connected.
	chosen := cfg.LiveModel
	if !config.ValidVoiceModel(chosen) {
		chosen = config.DefaultVoiceModel
	}
	models := make([]LiveModelView, 0, len(config.LiveVoiceModels))
	for _, m := range config.LiveVoiceModels {
		models = append(models, LiveModelView{Name: m.Name, Label: m.Label, Trait: m.Trait, Current: m.Name == chosen})
	}
	util.WriteJSON(w, map[string]any{"voices": list, "models": models})
}
