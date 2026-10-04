// brains.go holds GET /brains: which backends can answer for June on this machine, which of them the user is signed in to, and which one the daemon is configured to use. Every signal is read live — a login file on disk, a binary on PATH — so a brain nobody has set up says so instead of being offered.
package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"june/internal/agent"
	"june/internal/util"

	"june/internal/brain"
	"june/internal/config"
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

// errSettingsUnreadable is what Update answers when the config file is there but could not be read at all, which the window shows as it is.
var errSettingsUnreadable = errors.New("June could not read its settings file just now, so nothing was saved. Try again in a moment.")

// Update changes one setting and persists it, both under the lock, so no request goroutine reads the struct between the change and the copy that goes to disk. The change is made to the file as it is now, not to the copy the daemon loaded at startup: saving that copy put back every field changed on disk since — `june --autostart on` was reverted by the next voice pick, and the next start then removed the login entry the user had just asked for. Only once the save has succeeded does the daemon's own copy take the change, so a failed write leaves every reader on what is really on disk instead of on a value that silently reverts at the next start. Input: a function that sets fields on the config it is handed; it is called once for the file's copy and once for the daemon's, so it must not mutate anything the two share, such as a map. Output: errSettingsUnreadable, changing nothing, when the file is there but cannot be read; otherwise whatever the save function returned.
func (c *LiveConfig) Update(fn func(*config.JuneConfig)) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	// No read function means the daemon's own copy is the whole truth, which is what a test with a stub save hands in, so it never reads this machine's real file.
	onDisk := *c.cfg
	if c.read != nil {
		fresh, err := c.read()
		switch {
		case err == nil:
			onDisk = fresh
		case errors.Is(err, config.ErrUnreadable):
			// A file that will not parse is replaced, not left in the way: left in place it refused every save, so a user whose file broke could not even finish setup. The daemon's own copy, read at start before the file broke or made from it then by the same rules, is what goes in its place, and config.SaveConfig keeps the broken file beside it, so only the broken edit is lost, and that is kept. The next start says so once (see config.TakeSetAside).
			slog.Warn("june-config.json could not be read; saving June's own settings in its place", "error", err)
		case !errors.Is(err, fs.ErrNotExist):
			slog.Error("could not read june-config.json before saving a setting; nothing was saved", "error", err)
			return errSettingsUnreadable
		}
		// A file that is gone has nothing in it to keep, so the daemon's own copy is what is written, as it always was.
	}
	fn(&onDisk)
	if err := c.save(onDisk); err != nil {
		if errors.Is(err, config.ErrUnreadable) {
			slog.Error("june-config.json could not be read, nor kept aside; nothing was saved", "error", err)
			return errSettingsUnreadable
		}
		return err
	}
	fn(c.cfg)
	return nil
}

// automaticBrain is the id POST /brains takes to hand the choice back to the router, so no brain is picked and each ask goes to the best one this machine can run (see automaticBrainID).
const automaticBrain = "auto"

// loginRefreshWait is how long GET /brains?refresh=1 holds its answer for the login checks. `agy models` measured 6.5 seconds and the Claude and Codex checks allow themselves ten; a check still running past this lands in the next read instead.
const loginRefreshWait = 15 * time.Second

// loginRefreshFloor is how long after a round of login checks has finished a refresh=1 still answers from it instead of asking the providers again. It only has to absorb a double click or several screens mounting at once, since a refresh that arrives while a round runs shares that round; signing in again in a terminal takes longer than this, so the "Check again" after it always asks.
const loginRefreshFloor = 3 * time.Second

// pathRefreshFloor is how often freshPath reads Windows' Path again at most. A settings render checks PATH for several brains, and once covers them all.
const pathRefreshFloor = 2 * time.Second

// pathRead is when freshPath last read Windows' Path, as Unix nanoseconds.
var pathRead atomic.Int64

// freshPath adds to this process's PATH any folder Windows' lasting Path has gained since June started (see util.RefreshPath), at most once every pathRefreshFloor. It runs before the brains are looked for and before each ask, so Claude Code or Antigravity installed while June runs, during first-run setup above all, is found by the next look and answers the next question, without June being quit and opened again.
func freshPath() {
	now, last := time.Now().UnixNano(), pathRead.Load()
	if now-last < int64(pathRefreshFloor) || !pathRead.CompareAndSwap(last, now) {
		return
	}
	util.RefreshPath()
}

// loginCheckBound bounds a round of login checks as a whole. Each check has a timeout of its own; this keeps the wait on an `agy models`, the round's own or one an earlier read started, from outliving them when that read does not come back.
const loginCheckBound = 30 * time.Second

// loginRound is one round of login checks, shared by every refresh=1 that arrives while it runs. done is closed once it has finished; ended is when that was, written and read under loginRounds' lock.
type loginRound struct {
	done  chan struct{}
	ended time.Time
}

// loginRounds holds the latest round.
var loginRounds struct {
	sync.Mutex
	last *loginRound
}

// refreshLogins runs the brains' login health checks now instead of when their ten-minute polls next allow: the Claude usage read and the Codex profile check through limitsFor, and the Antigravity roster. First-run setup asks for this after the user signs in again in a terminal, so the row turns green then rather than up to ten minutes later. A refresh that arrives while a round runs waits for that round rather than starting another, and one within loginRefreshFloor of the last round's end answers from it.
// The round runs detached from the request, whose context bounds only the wait: each check marks its ten-minute poll spent before it asks, so a check the request's end cut short would leave a stale "login expired" standing for ten minutes.
// Input: the request's context, the usage lookup (nil skips Claude and Codex) and the PATH check. Output: none; it returns once the round is over, the request has gone, or loginRefreshWait has passed.
func refreshLogins(ctx context.Context, limitsFor BrainLimits, has func(string) bool) {
	freshPath()
	loginRounds.Lock()
	round := loginRounds.last
	if round == nil || (!round.ended.IsZero() && time.Since(round.ended) >= loginRefreshFloor) {
		round = &loginRound{done: make(chan struct{})}
		loginRounds.last = round
		go round.run(context.WithoutCancel(ctx), limitsFor, has)
	}
	loginRounds.Unlock()
	wait := time.NewTimer(loginRefreshWait)
	defer wait.Stop()
	select {
	case <-round.done:
	case <-ctx.Done():
	case <-wait.C:
	}
}

// run carries out one round: the ten-minute polls are forgotten, then every check runs side by side. Input: a context with no cancel of the request's, the usage lookup and the PATH check. Output: none; the checks leave their answers where /brains reads them.
func (round *loginRound) run(ctx context.Context, limitsFor BrainLimits, has func(string) bool) {
	ctx, cancel := context.WithTimeout(ctx, loginCheckBound)
	defer cancel()
	agent.ForgetLoginChecks()
	var wg sync.WaitGroup
	if limitsFor != nil {
		for _, id := range []string{"claude", "codex"} {
			wg.Go(func() { limitsFor(ctx, id) })
		}
	}
	if has("agy") {
		wg.Go(func() { reloadRoster(ctx, &agyCache, agyModels) })
	}
	wg.Wait()
	loginRounds.Lock()
	round.ended = time.Now()
	loginRounds.Unlock()
	close(round.done)
}

// reloadRoster reads a roster now, past its cache's TTL. A read the cache already has running is waited for instead of started again, since its answer is just as new and two `agy models` at once only cost twice. A read it starts runs on a goroutine of its own and is waited for the same way, so a read that never comes back ends the wait at ctx and not never: the round would otherwise never end, and every later refresh=1 would join it rather than ask again. Input: the context that bounds the wait, the cache, and how to read it. Output: none; the cache holds the answer.
func reloadRoster(ctx context.Context, c *modelCache, read func() []string) {
	c.mu.Lock()
	if !c.reading {
		c.reading = true
		go c.refresh(read)
	}
	c.mu.Unlock()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			c.mu.Lock()
			running := c.reading
			c.mu.Unlock()
			if !running {
				return
			}
		}
	}
}

// Brains builds the /brains handler. GET answers {"brains": [...], "automatic": bool} — the brains June knows about, and whether none is picked so the router chooses; GET ?refresh=1 runs the login checks first (see refreshLogins). POST {"brain": id, "model": string} picks one as the default and remembers its model, persists that to disk so it survives a restart, and answers with the same body GET would; with "default": false as well it only remembers the model, and the default brain stays where it was. POST {"brain": "auto"} (or "") picks none again: the configured brain and its model are cleared, each brain's remembered model is kept, and the router goes back to its own rank order; with "default": false it is 400, as there is no brain to keep that model for. An id outside the known ones is 400 and changes nothing, and so is an id whose provider has no backend in internal/brain, with the reason in the body. Input: the config accessor shared with the rest of the daemon, so a POST's change is visible everywhere and no two request goroutines touch the struct at once, and the usage lookup for the rows' limit bars. Output: the handler.
func Brains(cfg *LiveConfig, limitsFor BrainLimits) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			freshPath()
			if r.URL.Query().Get("refresh") == "1" {
				refreshLogins(r.Context(), limitsFor, onPath)
			}
			writeBrains(r.Context(), w, cfg.Get(), limitsFor)
		case http.MethodPost:
			var req struct {
				Brain string `json:"brain"`
				Model string `json:"model"`
				// Default is false only when the body says so: the Settings model chip sends it to store a model without moving the default brain, and the header's brain picker leaves it out.
				Default *bool `json:"default"`
			}
			if !DecodeJSON(w, r, &req) {
				return
			}
			// Once a brain had been picked there was no way back to "none picked": every id here wrote a concrete provider, so the rank-order routing a fresh install starts on was gone for good unless june-config.json was edited by hand, which the running daemon then did not know about.
			if req.Brain == automaticBrain || req.Brain == "" {
				// A model-only save has to name the brain the model is for. Read as "automatic", a chip that sent an empty id would quietly unpin the brain the user picked.
				if req.Default != nil && !*req.Default {
					http.Error(w, "no brain named to keep the model for", http.StatusBadRequest)
					return
				}
				if err := cfg.Update(func(c *config.JuneConfig) {
					c.Brain.Provider, c.Brain.Model = "", ""
				}); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				agent.SetPreferredProvider("")
				writeBrains(r.Context(), w, cfg.Get(), limitsFor)
				return
			}
			provider, ok := providerForBrainID(req.Brain)
			if !ok {
				http.Error(w, "unknown brain: "+req.Brain, http.StatusBadRequest)
				return
			}
			// A brain internal/brain has no code to answer with is refused here, not merely greyed out in the list GET returns. Accepting it persisted the provider and FromConfig then failed every call to it with ErrNoBackend, so every duty pinned to it was answered by whichever provider the router offered next, which is a provider the user did not choose.
			if note := brain.NoBackendNote(provider); note != "" {
				http.Error(w, "cannot pick "+req.Brain+": "+note, http.StatusBadRequest)
				return
			}
			makeDefault := req.Default == nil || *req.Default
			err := cfg.Update(func(c *config.JuneConfig) {
				if makeDefault {
					c.Brain.Provider = provider
				}
				// A model picked for the default brain is the model the daemon calls it with.
				if c.Brain.Provider == provider {
					c.Brain.Model = req.Model
				}
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
			if makeDefault {
				// The router answers a question with the picked brain first from this moment, not from the next daemon start — and only once the pick is on disk, so a save that failed leaves the router on the brain the config still names.
				agent.SetPreferredProvider(provider)
			}
			writeBrains(r.Context(), w, cfg.Get(), limitsFor)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// writeBrains writes the brain rows for cfg as JSON, the body both GET and POST /brains answer with. automatic is true while no brain is picked, which is what lets the picker mark its Automatic entry rather than the row the router happens to land on.
func writeBrains(ctx context.Context, w http.ResponseWriter, cfg config.JuneConfig, limitsFor BrainLimits) {
	home, _ := os.UserHomeDir()
	util.WriteJSON(w, map[string]any{"brains": brainList(ctx, cfg, home, onPath, limitsFor), "automatic": cfg.Brain.Provider == ""})
}

// providerForBrainID maps a brain id to the BrainConfig provider that should answer June's one-shot duties when that brain is picked as the default, using only the provider constants config.go declares. Every id gets its own distinct provider, so POST /brains never persists a different brain's provider under this one's name — codex's row answers for real once internal/brain.FromConfig is given an asker to call, and ollama's has no backend at all, which is why brainList marks that row unavailable rather than letting a pick land on a provider that cannot answer. ok is false when id names none of the brains June knows about, and the caller must leave the config untouched in that case.
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

// brainIDs are the brains June knows about, in the order the picker draws them. GET /usage keys its per-provider allowance windows by the same ids, so the settings page and the picker name a brain the same way.
// Claude is last on purpose: that subscription is the user's own coding workhorse, so June treats it as the fallback the ask path already makes it (Gemini, then Codex, then Claude — see internal/agent/ask.go), not as the brain a picker offers first.
var brainIDs = []string{"antigravity", "gemini", "codex", "grok", "ollama", "claude"}

// onPath reports whether a binary of that name can be run from this process's PATH.
func onPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// brainList builds the rows. Input: a context for the limits lookup, the config (for the default brain and the Gemini model), the home directory the login files live under, the PATH check — both injected so the tests read a temporary home and never shell out — and the allowance lookup, which may be nil. Output: the rows in the order the window draws them, with signed_in false on any brain internal/brain cannot actually answer with and limits_note saying why.
func brainList(ctx context.Context, cfg config.JuneConfig, home string, has func(string) bool, limitsFor BrainLimits) []BrainView {
	def := defaultBrainID(cfg.Brain.Provider)
	claudeAccount := plainField(agent.ClaudeCredentialsPath(home), "claudeAiOauth", "subscriptionType")
	codexAccount := plainField(agent.CodexAuthPath(home), "", "auth_mode")

	// Both rosters come out of the cache rather than off the command line: `agy models` takes about three and a half seconds, and this route runs on every settings render and again after every pick.
	ollamaModels := []string{}
	if has("ollama") {
		ollamaModels, _ = ollamaCache.get(ollamaList)
	}
	agyList := []string{}
	if has("agy") {
		agyList, _ = agyCache.get(agyModels)
	}
	// The roster is also the Antigravity login's health check. `agy models` fails the same UNAUTHENTICATED eligibility check an ask does when the login has expired, and agyModels records that refusal. Measured on 2026-09-15, when the picker drew the row as available for hours while every ask to it failed. A read that failed any other way, or that nobody has made yet, is not evidence, so the row keeps its binary-presence answer.
	agySignedOut := has("agy") && agyRefused.Load()
	agyNote := ""
	if agySignedOut {
		agyNote = "the Antigravity login has expired: open Antigravity and sign in again"
	}

	list := []BrainView{
		{
			ID:         "antigravity",
			Name:       "Antigravity",
			SignedIn:   loggedIn("antigravity", home, has),
			Models:     agyList,
			LimitsNote: agyNote,
			Note:       "Antigravity answers under the Google plan you already pay for, so nothing here counts against a Gemini key. The models are the ones your plan offers, which include Claude and GPT-OSS as well as Gemini.",
		},
		{
			ID:       "gemini",
			Name:     "Gemini",
			SignedIn: os.Getenv("GEMINI_API_KEY") != "",
			Models:   geminiModels(cfg.Brain),
			Note:     "Gemini answers with your Gemini key. A free key allows a set number of questions a day, which a busy day can use up.",
		},
		{
			ID:       "codex",
			Name:     "Codex",
			SignedIn: loggedIn("codex", home, has),
			Account:  codexAccount,
			Models:   []string{"gpt-5.5", "gpt-5.6-luna"},
			Note:     "Codex answers under the ChatGPT plan you already pay for, using the sign-in from the Codex app.",
		},
		{
			ID:       "grok",
			Name:     "Grok",
			SignedIn: has("grok"),
			Models:   []string{},
			Note:     "Grok answers under the plan you already pay for. It offers no choice of model, so there is nothing to pick here.",
		},
		{
			ID:       "ollama",
			Name:     "Ollama",
			SignedIn: has("ollama"),
			Models:   ollamaModels,
			Note:     "Ollama runs a model on this computer, so nothing leaves it. The models are the ones Ollama has installed.",
		},
		{
			ID:       "claude",
			Name:     "Claude",
			SignedIn: loggedIn("claude", home, has),
			Account:  claudeAccount,
			Models:   []string{"haiku", "sonnet", "opus"},
			Note:     "Claude answers through Claude Code, under the subscription you already pay for. June asks it last, so the plan you work with is spent only when the others are out.",
		},
	}

	for i := range list {
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
		// The provider itself refused this login when its usage was last read, which it can only have done for a credential that no longer works. The row says so and cannot be picked, rather than waiting for the first question to die.
		if snap.SignedOut {
			list[i].SignedIn = false
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
	// No brain picked: the row marked default is the one the router actually answers on, which the header must name — it showed "Gemini" with no key while every ask was answered elsewhere or failed. When nothing can answer at all no row is marked, rather than a Gemini row that has no key. Read after the loop above, which is what turns off a row whose login the provider has refused.
	answering := def
	if cfg.Brain.Provider == "" {
		answering = automaticBrainID(func(id string) bool {
			i := slices.IndexFunc(list, func(b BrainView) bool { return b.ID == id })
			return i >= 0 && list[i].SignedIn
		})
	}
	for i := range list {
		list[i].Default = list[i].ID == answering
		// def, not answering: a model the config carries with no provider is a Gemini model, and showing it on the subscription row the router lands on would name a model that brain does not have.
		list[i].Model = modelFor(list[i].ID, cfg, def)
	}
	return list
}

// automaticRank is the order the router tries the subscription brains in when none is picked and there is no Gemini key (see the cards in internal/agent/router.go), by the ids this file uses. Grok answers no ask, so it is not in it.
var automaticRank = []string{"codex", "antigravity", "claude"}

// automaticBrainID is the brain the router answers on while none is picked, so GET /brains and GET /settings name the same one. Input: whether a brain, by id, is signed in on this machine. Output: "gemini" while there is a key, since the router ranks it first; otherwise the first signed-in brain of automaticRank; "" when nothing can answer at all.
func automaticBrainID(signedIn func(id string) bool) string {
	if os.Getenv("GEMINI_API_KEY") != "" {
		return "gemini"
	}
	for _, id := range automaticRank {
		if signedIn(id) {
			return id
		}
	}
	return ""
}

// loggedIn is what this machine itself says about a subscription brain's login, read the same way for its /brains row and for the Settings line naming the brain that answers: a login file for Codex, for Antigravity its binary on PATH unless the last roster read was refused, and for Claude both, since an ask runs the claude command and a login file left behind by an uninstall, or one written before June's PATH could see the program, answered nothing. Input: the brain id, the home directory the login files live under, and the PATH check. Output: false for any other id.
func loggedIn(id, home string, has func(string) bool) bool {
	switch id {
	case "codex":
		return util.Exists(agent.CodexAuthPath(home))
	case "antigravity":
		return has("agy") && !agyRefused.Load()
	case "claude":
		// The file alone is not a login: a dead OAuth session leaves it in place with both tokens emptied.
		return has("claude") && agent.ClaudeSignedIn(home)
	}
	return false
}

// brainSignedIn is a brain's signed_in as its /brains row ends up with it, for a caller that needs the answer without building every row: loggedIn, less a login the provider itself refused when its usage was last read. An expired Codex login keeps its file, so without the second half Settings named Codex as the brain answering while the picker had already greyed it out. Input: a context for the lookup, the brain id, the home directory, the PATH check, and the usage lookup, which may be nil. Output: whether the brain can be answered on.
func brainSignedIn(ctx context.Context, id, home string, has func(string) bool, limitsFor BrainLimits) bool {
	if !loggedIn(id, home, has) {
		return false
	}
	if limitsFor == nil {
		return true
	}
	snap, ok := limitsFor(ctx, id)
	return !ok || !snap.SignedOut
}

// modelFor is the model to show already chosen for a brain row. Input: the row's id, the config, and the id of the default brain. Output: whatever POST /brains last set for that id; failing that, the model BrainConfig itself carries when this row is the one the daemon is actually configured to use, so a config written before BrainModels existed still shows correctly; otherwise "".
func modelFor(id string, cfg config.JuneConfig, def string) string {
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

// agyRefused says the last `agy models` run was refused with UNAUTHENTICATED, which is the one failure that means the Antigravity login is gone. agyModels writes it; brainList reads it.
var agyRefused atomic.Bool

// agyModels asks the Antigravity command line which models the user's plan can call, and records in agyRefused whether it refused the login. Output: the model ids in the order it lists them, or an empty list when the command fails or reports none.
// The output is two columns, the id and a human label — "gemini-3.8-flash-high     Gemini 3.8 Flash (High)" — with no header row, so every non-blank line's first field is an id. The roster is read live rather than hardcoded because it is the user's own plan that decides what is in it, and it grows.
func agyModels() []string {
	ctx, cancel := context.WithTimeout(context.Background(), agyModelsTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "agy", "models")
	// In a process group (a Job Object on Windows) of its own, so a timeout ends the helpers agy starts along with agy itself: killed alone, agy left them running, outside any job, past the daemon's own exit. internal/agent/agy.go runs agy the same way.
	util.OwnProcessGroup(cmd)
	cmd.Cancel = func() error { return util.KillProcessGroup(cmd) }
	// A helper that outlives the kill all the same keeps stdout open, which would hold the wait, and the cache's read with it, past the timeout for good.
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	release, err := util.StartProcessGroup(cmd)
	if err == nil {
		err = cmd.Wait()
		release()
	}
	out := stdout.String()
	// UNAUTHENTICATED is a gRPC status code rather than prose, so it is not part of any model id or label.
	refused := strings.Contains(out+stderr.String(), "UNAUTHENTICATED")
	agyRefused.Store(refused)
	if err != nil || refused {
		slog.Warn("could not read the Antigravity model roster", "error", err, "refused", refused)
		return []string{}
	}
	// Every line is "id\tLabel"; the ids carry the effort agy will run at, as a -high, -medium or -low suffix, so picking a model here is also picking the effort.
	return firstFields(out, false)
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
	cmd := exec.CommandContext(ctx, "ollama", "list")
	util.HideConsole(cmd)
	out, err := cmd.Output()
	if err != nil {
		return []string{}
	}
	// The first line is the header row ("NAME  ID  SIZE  MODIFIED"); every later line starts with the model's name.
	return firstFields(string(out), true)
}
