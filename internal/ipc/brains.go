// brains.go holds GET /brains: which backends can answer for Ora on this machine, which of them the user is signed in to, and which one the daemon is configured to use. Every signal is read live — a login file on disk, a binary on PATH — so a brain nobody has set up says so instead of being offered.
package ipc

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"net/http"
	"ora/internal/agent"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"ora/internal/brain"
	"ora/internal/config"
)

// ollamaListTimeout bounds the `ollama list` call this route makes, so a wedged local server cannot hang the window's settings screen.
const ollamaListTimeout = 3 * time.Second

// agyModelsTimeout is how long `agy models` is given. It is not the three seconds a local Ollama gets, because agy fetches its roster over the network: measured 6.5 seconds on 2026-09-07, so a three-second deadline killed every call and the Antigravity row published an empty model list for as long as it existed. The read runs in the background off a ten-minute cache, so a generous deadline costs a request nothing.
const agyModelsTimeout = 30 * time.Second

// BrainView is one backend on GET or POST /brains. SignedIn says whether this machine can actually call it; Account is what the login file plainly says about the account (a plan name, a mode), never a token; Models are the models the user may pick, empty for a CLI that exposes no choice; Model is the one currently chosen for this brain, "" when none has ever been picked; Note is one sentence about why this brain is here; Default marks the one the daemon is configured to use.
type BrainView struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	SignedIn bool     `json:"signed_in"`
	Account  string   `json:"account"`
	Models   []string `json:"models"`
	Model    string   `json:"model"`
	Note     string   `json:"note"`
	Default  bool     `json:"default"`
	// Limits are the allowance windows this brain's provider exposes for the user's own account — a five-hour or weekly subscription window, a daily request ceiling — which is what the picker draws a mini bar from. Empty, never null, for a provider that exposes none or that nothing has been read from yet.
	Limits []brain.UsageLimit `json:"limits"`
	// LimitsAt is when Limits was read, RFC 3339, and "" when there are none.
	LimitsAt string `json:"limits_at"`
	// LimitsNote explains an empty Limits when that emptiness was checked rather than merely never read — e.g. Grok's CLI has no usage reading to give at all — so the picker can show that fact instead of a plain "no usage data" placeholder. "" when Limits may yet be filled in.
	LimitsNote string `json:"limits_note"`
}

// BrainLimits is where /brains reads a brain's allowance windows. Input: a context, so a lookup that has to ask a provider can be cut short, and the brain id. Output: the newest reading and true, or false when that brain exposes no allowance or none has been read yet. nil is the same as a lookup that always says false, which is what the tests and a daemon with no usage store pass.
type BrainLimits func(ctx context.Context, brainID string) (brain.UsageSnapshot, bool)

// Update changes the config in place and persists it, both under the lock, so no request goroutine reads the struct between the change and the copy that goes to disk. Input: a function that edits the config it is handed. Output: whatever the save function returned.
func (c *LiveConfig) Update(fn func(*config.OraConfig)) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	fn(c.cfg)
	return c.save(*c.cfg)
}

// Brains builds the /brains handler. GET answers the brains Ora knows about as JSON. POST {"brain": id, "model": string} picks one as the default and remembers its model, persists that to disk so it survives a restart, and answers with the same list GET would. An id outside the known ones is 400 and changes nothing, and so is an id whose provider has no backend in internal/brain, with the reason in the body. Input: the config accessor shared with the rest of the daemon, so a POST's change is visible everywhere and no two request goroutines touch the struct at once, and the usage lookup for the rows' limit bars. Output: the handler.
func Brains(cfg *LiveConfig, limitsFor BrainLimits) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeBrains(r.Context(), w, cfg.Get(), limitsFor)
		case http.MethodPost:
			var req struct {
				Brain string `json:"brain"`
				Model string `json:"model"`
			}
			if !DecodeJSON(w, r, &req) {
				return
			}
			provider, ok := providerForBrainID(req.Brain)
			if !ok {
				http.Error(w, "unknown brain: "+req.Brain, http.StatusBadRequest)
				return
			}
			// A brain internal/brain has no code to answer with is refused here, not merely greyed out in the list GET returns. Accepting it persisted the provider, FromConfig then failed every call with ErrNoBackend, and WithCodexFallback answered each one on Codex — which is the "answering on a provider they did not choose" the ErrNoBackend change was written to stop, moved from Gemini to Codex.
			if note := brain.NoBackendNote(provider); note != "" {
				http.Error(w, "cannot pick "+req.Brain+": "+note, http.StatusBadRequest)
				return
			}
			// The router answers a question with the picked brain first from this moment, not from the next daemon start.
			agent.SetPreferredProvider(provider)
			err := cfg.Update(func(c *config.OraConfig) {
				c.Brain.Provider = provider
				c.Brain.Model = req.Model
				// Get hands out shallow copies that share this map, and a GET may be reading one of them right now, so the map is replaced rather than written into: a Go map read and written at once is a fatal runtime error, not a race a lock elsewhere could tolerate.
				models := maps.Clone(c.BrainModels)
				if models == nil {
					models = map[string]string{}
				}
				models[req.Brain] = req.Model
				c.BrainModels = models
			})
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeBrains(r.Context(), w, cfg.Get(), limitsFor)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// writeBrains writes the brain rows for cfg as JSON, the body both GET and POST /brains answer with.
func writeBrains(ctx context.Context, w http.ResponseWriter, cfg config.OraConfig, limitsFor BrainLimits) {
	home, _ := os.UserHomeDir()
	writeJSON(w, map[string]any{"brains": brainList(ctx, cfg, home, onPath, limitsFor)})
}

// providerForBrainID maps a brain id to the BrainConfig provider that should answer ORA's one-shot duties when that brain is picked as the default, using only the provider constants config.go declares. Every id gets its own distinct provider, so POST /brains never persists a different brain's provider under this one's name — codex's row answers for real once internal/brain.FromConfig is given an asker to call, and ollama's has no backend at all, which is why brainList marks that row unavailable rather than letting a pick land on a provider that cannot answer. ok is false when id names none of the brains Ora knows about, and the caller must leave the config untouched in that case.
func providerForBrainID(id string) (provider string, ok bool) {
	switch id {
	case "claude":
		return config.BrainClaudeCLI, true
	case "grok":
		return config.BrainGrokCLI, true
	case "codex":
		return config.BrainCodex, true
	case "ollama":
		return config.BrainOllama, true
	case "gemini":
		return config.BrainGeminiAPI, true
	case "antigravity":
		return config.BrainAgyCLI, true
	default:
		return "", false
	}
}

// brainIDs are the brains Ora knows about, in the order the picker draws them. GET /usage keys its per-provider allowance windows by the same ids, so the settings page and the picker name a brain the same way.
// Claude is last on purpose: that subscription is the user's own coding workhorse, so Ora treats it as the fallback the ask path already makes it (Gemini, then Codex, then Claude — see internal/agent/ask.go), not as the brain a picker offers first.
var brainIDs = []string{"antigravity", "gemini", "codex", "grok", "ollama", "claude"}

// onPath reports whether a binary of that name can be run from this process's PATH.
func onPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// brainList builds the rows. Input: a context for the limits lookup, the config (for the default brain and the Gemini model), the home directory the login files live under, the PATH check — both injected so the tests read a temporary home and never shell out — and the allowance lookup, which may be nil. Output: the rows in the order the window draws them, with signed_in false on any brain internal/brain cannot actually answer with and limits_note saying why.
func brainList(ctx context.Context, cfg config.OraConfig, home string, has func(string) bool, limitsFor BrainLimits) []BrainView {
	def := defaultBrainID(cfg.Brain.Provider)
	claudeAccount := plainField(filepath.Join(home, ".claude", ".credentials.json"), "claudeAiOauth", "subscriptionType")
	codexAccount := plainField(filepath.Join(home, ".codex", "auth.json"), "", "auth_mode")

	// Both rosters come out of the cache rather than off the command line: `agy models` takes about three and a half seconds, and this route runs on every settings render and again after every pick.
	ollamaModels := []string{}
	if has("ollama") {
		ollamaModels = ollamaCache.get(ollamaList)
	}
	agyList := []string{}
	if has("agy") {
		agyList = agyCache.get(agyModels)
	}

	list := []BrainView{
		{
			ID:       "antigravity",
			Name:     "Antigravity",
			SignedIn: has("agy"),
			Models:   agyList,
			Note:     "Antigravity runs models under the Google plan the user already pays for, so a duty answered here costs nothing against the metered API key. The list is whatever `agy models` reports, which on this plan is more than Gemini — Claude and GPT-OSS are in it too.",
		},
		{
			ID:       "gemini",
			Name:     "Gemini",
			SignedIn: os.Getenv("GEMINI_API_KEY") != "",
			Models:   geminiModels(cfg.Brain),
			Note:     "Gemini answers on the metered API key in ~/.config/ora/env, which has a free tier that a day of duties can run through; the model comes from ora-config.json.",
		},
		{
			ID:       "codex",
			Name:     "Codex",
			SignedIn: exists(filepath.Join(home, ".codex", "auth.json")),
			Account:  codexAccount,
			Models:   []string{"gpt-5.5", "gpt-5.6-luna"},
			Note:     "OpenAI endorses using a Codex login from open-source harnesses, so Ora may call it under the plan the user already pays for.",
		},
		{
			ID:         "grok",
			Name:       "Grok",
			SignedIn:   has("grok"),
			Models:     []string{},
			Note:       "The Grok command line exposes no model choice, so there is nothing to pick here.",
			LimitsNote: brain.GrokNote(),
		},
		{
			ID:       "ollama",
			Name:     "Ollama",
			SignedIn: has("ollama"),
			Models:   ollamaModels,
			Note:     "Ollama runs a model on this machine, so the list is whatever `ollama list` reports and nothing leaves the laptop.",
		},
		{
			ID:       "claude",
			Name:     "Claude",
			SignedIn: exists(filepath.Join(home, ".claude", ".credentials.json")),
			Account:  claudeAccount,
			Models:   []string{"haiku", "sonnet", "opus"},
			Note:     "Ora runs Claude through Anthropic's own command line, because a third-party login is billed as extra usage on top of the subscription. It is drawn last because the same subscription is the user's own coding workhorse, so Ora spends it only when the others are out.",
		},
	}

	for i := range list {
		list[i].Default = list[i].ID == def
		list[i].Model = modelFor(list[i].ID, cfg, def)
		list[i].Limits = []brain.UsageLimit{}
		// A brain internal/brain has no code to answer with is never offered as pickable, whatever the machine says about it: internal/brain.FromConfig fails every call to it with ErrNoBackend, and before that it answered on the Gemini API instead, which spent the metered free tier the user picked another brain to avoid.
		if provider, ok := providerForBrainID(list[i].ID); ok {
			if note := brain.NoBackendNote(provider); note != "" {
				list[i].SignedIn = false
				list[i].LimitsNote = note
			}
		}
		// The claude row's own usage bars come from an undocumented Anthropic endpoint (see internal/agent's RefreshClaudeUsage), which the user may turn off in Settings; when they have, no fetch runs and this says why rather than leaving the row looking like nothing has been read yet.
		if list[i].ID == "claude" && !cfg.ClaudeUsageFromLoginEnabled() {
			list[i].LimitsNote = "turned off in Settings"
		}
		if limitsFor == nil {
			continue
		}
		snap, ok := limitsFor(ctx, list[i].ID)
		if !ok {
			continue
		}
		if len(snap.Limits) > 0 {
			list[i].Limits = snap.Limits
			list[i].LimitsAt = rfc3339(snap.At)
		}
		// A caveat on the reading itself — today, that a Gemini model's ceiling is a default because nobody has measured that model — is what the row's note says, since it explains the bar the picker is already drawing.
		if snap.Note != "" {
			list[i].LimitsNote = snap.Note
		}
	}
	return list
}

// modelFor is the model to show already chosen for a brain row. Input: the row's id, the config, and the id of the default brain. Output: whatever POST /brains last set for that id; failing that, the model BrainConfig itself carries when this row is the one the daemon is actually configured to use, so a config written before BrainModels existed still shows correctly; otherwise "".
func modelFor(id string, cfg config.OraConfig, def string) string {
	if m := cfg.BrainModels[id]; m != "" {
		return m
	}
	if id == def {
		return cfg.Brain.Model
	}
	return ""
}

// defaultBrainID maps a configured provider to one of the five ids. Input: config.BrainConfig.Provider. Output: the id of the brain that answers today — "gemini" for the Gemini API, for the Antigravity CLI (the same model family), and for any provider string this switch does not recognise.
func defaultBrainID(provider string) string {
	switch provider {
	case config.BrainClaudeCLI:
		return "claude"
	case config.BrainGrokCLI:
		return "grok"
	case config.BrainCodex:
		return "codex"
	case config.BrainOllama:
		return "ollama"
	case config.BrainAgyCLI:
		return "antigravity"
	default:
		return "gemini"
	}
}

// geminiModels is the Gemini models the window may offer: the one the config pins, when it pins one, and the model the daemon otherwise calls. A model pinned while a non-Gemini provider is configured belongs to that provider's own namespace (a Claude model alias, a Grok or Codex model name, an Ollama tag), not Gemini's, so it is left out here.
func geminiModels(cfg config.BrainConfig) []string {
	models := []string{config.TextModel}
	nonGemini := cfg.Provider == config.BrainClaudeCLI || cfg.Provider == config.BrainGrokCLI || cfg.Provider == config.BrainCodex || cfg.Provider == config.BrainOllama
	if cfg.Model != "" && cfg.Model != config.TextModel && !nonGemini {
		models = append([]string{cfg.Model}, models...)
	}
	return models
}

// exists reports whether a path is there at all, which is what "signed in" means for a login file.
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// plainField reads one plain string field out of a JSON login file, so the window can name the account without ever handling a credential. Input: the file, the object to look inside ("" for the top level), and the field. Output: the field's value, or "" when the file is missing, unreadable, not JSON, or the field is not a plain string. Nothing here is logged.
func plainField(path, object, field string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var doc map[string]any
	if json.Unmarshal(data, &doc) != nil {
		return ""
	}
	if object != "" {
		inner, ok := doc[object].(map[string]any)
		if !ok {
			return ""
		}
		doc = inner
	}
	value, _ := doc[field].(string)
	return value
}

// agyModels asks the Antigravity command line which models the user's plan can call. Output: the model ids in the order it lists them, or an empty list when the command fails or reports none.
// The output is two columns, the id and a human label — "gemini-3.8-flash-high     Gemini 3.8 Flash (High)" — with no header row, so every non-blank line's first field is an id. The roster is read live rather than hardcoded because it is the user's own plan that decides what is in it, and it grows.
func agyModels() []string {
	ctx, cancel := context.WithTimeout(context.Background(), agyModelsTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "agy", "models").Output()
	if err != nil {
		slog.Warn("could not read the Antigravity model roster", "error", err)
		return []string{}
	}
	// Every line is "id\tLabel"; the ids carry the effort agy will run at, as a -high, -medium or -low suffix, so picking a model here is also picking the effort.
	return firstFields(string(out), false)
}

// firstFields reads one id per line off a command's table output. Input: the output, and whether its first line is a header to skip. Output: the first whitespace-separated field of every other non-blank line.
func firstFields(out string, header bool) []string {
	ids := []string{}
	for i, line := range strings.Split(out, "\n") {
		if (header && i == 0) || strings.TrimSpace(line) == "" {
			continue
		}
		ids = append(ids, strings.Fields(line)[0])
	}
	return ids
}

// ollamaList asks the local Ollama for its installed models. Output: the model names in the order it lists them, or an empty list when the command fails or reports none.
func ollamaList() []string {
	ctx, cancel := context.WithTimeout(context.Background(), ollamaListTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ollama", "list").Output()
	if err != nil {
		return []string{}
	}
	// The first line is the header row ("NAME  ID  SIZE  MODIFIED"); every later line starts with the model's name.
	return firstFields(string(out), true)
}
