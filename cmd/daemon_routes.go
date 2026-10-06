package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"june/internal/agent"
	"june/internal/audio"
	"june/internal/brain"
	"june/internal/components"
	"june/internal/config"
	"june/internal/db"
	"june/internal/ipc"
	"june/internal/proactive"
	"june/internal/recorder"
	"june/internal/tracker"
	"june/internal/update"
	"june/internal/util"
)

type routeDependencies struct {
	// ctx is the daemon's own context, which the update checker runs on.
	ctx             context.Context
	auth            func(http.HandlerFunc) http.HandlerFunc
	daemon          *tracker.Daemon
	ipcServer       *ipc.Server
	actJobs         *ipc.ActJobs
	store           *db.Store
	apiKey          string
	meetingRecorder *recorder.Recorder
	scheduler       *proactive.Scheduler
	liveConfig      *ipc.LiveConfig
	brainLimits     ipc.BrainLimits
	appConfig       config.JuneConfig
	startTime       time.Time
	// features is the local-features downloader, made by startDaemonServices because its shutdown stops it.
	features *components.Service
	// exeDir is the directory June's own programs are in.
	exeDir string
	// restartBlocker names what a restart now would cut short (see the function of that name).
	restartBlocker func() string
	// busy is what restartBlocker reads besides the recorder and the downloader; the routes fill in the voice session's and the dictation's parts.
	busy *busyState
	// hotkeys is the shortcut record, made by startDaemonServices because its shutdown lets go of the shortcut. A second daemon on a port of its own (a test, a dry run) gets one that leaves the desktop's shortcut alone (see ipc.Hotkeys.HandsOff).
	hotkeys *ipc.Hotkeys
}

// featureStopBound is how long the shutdown waits for local-feature jobs to stop. Moving a component into place is a rename on one volume, so the bound is for an archive part-way through being unpacked or a smoke test just started.
const featureStopBound = 30 * time.Second

// restartBlocker names what restarting June now would cut short: "recording" while a meeting is being recorded, "processing" while one is still being transcribed and written up, "downloading" while local features are downloading or installing, then what the user is in the middle of (see busyState): "voice" while a voice conversation is open, "dictating" while a dictation is, "acting" while a computer task is running, paused or waiting on an answer, "asking" while an answer is being written; and "" when nothing would. Input: the local-features downloader and the daemon's busy state. Output: the function, read at each request.
// A restart cuts each of them short and the June after it takes none of them up again: the rest of the call goes unrecorded, the write-up waits for the next start's sweep, a download stops where it is, and a conversation, a dictation, a task or an answer is simply gone. It is what POST /restart, setup's restarts (ipc.SetupDeps.RestartBlocker), the restart setup puts off until nothing is in the way, and the updater all ask, so each of them waits for the user rather than cutting in on them.
func restartBlocker(features *components.Service, busy *busyState) func() string {
	return func() string {
		switch {
		case meetingRecorder != nil && meetingRecorder.Active():
			return "recording"
		case meetingRecorder != nil && !meetingRecorder.Quiescent():
			return "processing"
		case features.Busy():
			return "downloading"
		}
		return busy.busyWord()
	}
}

// withPreflight answers every CORS preflight in front of the mux, and hands everything else straight to it. Input: the routes and the same auth wrapper they are registered with, which is what writes the CORS headers and answers an OPTIONS 204 without asking for a token. Output: the handler the daemon serves.
//
// It exists because a route whose pattern names a method — "POST /notices/{kind}/{id}/action" — is routed to that method alone, so the OPTIONS a browser sends first never reaches the handler and the mux answers it 405 with no CORS headers of its own. The browser then blocks the real request, the window's fetch rejects, and the card says "Could not do that" while the daemon logs nothing at all, because the POST was never sent. Every notice button was dead this way, and so was every /act and /open route.
//
// In front of the mux rather than as a route of its own: Go's mux refuses an "OPTIONS /" pattern as ambiguous against every plain pattern like "/context", which matches all methods on a narrower path. Being in front also means a route added later cannot reintroduce this by forgetting anything.
func withPreflight(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc) http.Handler {
	preflight := auth(func(w http.ResponseWriter, r *http.Request) {})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			preflight(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// registerDaemonRoutes wires up the HTTP routes on mux for IPC communication.
func registerDaemonRoutes(mux *http.ServeMux, d routeDependencies) {
	auth := d.auth
	daemon := d.daemon
	ipcServer := d.ipcServer
	actJobs := d.actJobs
	store := d.store
	apiKey := d.apiKey
	meetingRecorder := d.meetingRecorder
	scheduler := d.scheduler
	liveConfig := d.liveConfig
	brainLimits := d.brainLimits
	appConfig := d.appConfig
	startTime := d.startTime

	// heartbeat — deliberately unauthenticated: root.go's pre-spawn liveness probe polls this before it can assume the token file even exists yet, and the build identity it returns reveals nothing sensitive.
	mux.HandleFunc("/ping", pingHandler)

	// The tray asks the window it started to open or to show its hover; the instruction travels on the event stream the window already reads. An open asked for before the window listens is kept until it does (see openWhenListening); with the window turned off there is none to wait for.
	keepOpen := func() {
		if appConfig.Window {
			openWhenListening(d.ctx, ipcServer)
		}
	}
	mux.HandleFunc("/window", auth(windowRoute(ipcServer.Window, keepOpen)))

	// pause/resume tracking. These two, /ask, /dictate/start and /dictate/stop and /overlay name their method in the pattern, so a GET (a link, a prefetch, a probe) never pauses tracking, starts an ask or opens the microphone; the mux answers any other method 405 with an Allow header, and withPreflight still answers the CORS preflight in front of it. The window and the hover POST to these, and the tray does too (authedDaemonPost). The routes further down that change something check their method inside the handler instead, and the rest only read; /window above checks none, because the tray's Open item, a notice's "Open in June" and the CLI's own launch all open the window with authedDaemonGet.
	// Both are kept on disk as well, so the user's pause outlasts the restarts June now makes on its own account (see pauseControl).
	// POST /pause takes an optional {"minutes": N}, 1 to maxPauseMinutes: observation comes back by itself after that long. No body, or no minutes, pauses until the user resumes, which is what the trays send. Answers "paused", or 400 with why for a body it cannot use.
	mux.HandleFunc("POST /pause", auth(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Minutes int `json:"minutes"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, `the body must be JSON like {"minutes": 60}, or empty to pause until resumed: `+err.Error(), http.StatusBadRequest)
			return
		}
		if req.Minutes < 0 || req.Minutes > maxPauseMinutes {
			http.Error(w, fmt.Sprintf("minutes must be between 1 and %d, or left out to pause until resumed", maxPauseMinutes), http.StatusBadRequest)
			return
		}
		var until time.Time
		if req.Minutes > 0 {
			until = time.Now().Add(time.Duration(req.Minutes) * time.Minute)
		}
		pauses.pause(until)
		slog.Info("tracking paused via IPC", "minutes", req.Minutes)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("paused"))
	}))

	// While observation waits for first-run setup POST /resume answers 409 {"error":"setup_not_done"}: nothing is observed until then, as the installer's privacy notice promises, and finishing setup is what turns observation on. The trays open the window on setup instead (see resumeFromTray). With the window turned off there is no setup to finish, so a resume is the user's own consent and is taken (see setupWaits).
	mux.HandleFunc("POST /resume", auth(func(w http.ResponseWriter, r *http.Request) {
		if setupWaits() {
			writeLifecycleJSON(w, http.StatusConflict, map[string]string{"error": "setup_not_done", "message": "June starts watching once setup is finished."})
			return
		}
		pauses.resume()
		slog.Info("tracking resumed via IPC")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("resumed"))
	}))

	// GET /status: {"paused": bool, "paused_until": string}, paused_until the RFC 3339 time a pause for a while ends, "" when observation is on or paused until the user resumes.
	mux.HandleFunc("GET /status", auth(func(w http.ResponseWriter, r *http.Request) {
		paused, until := daemon.IsPaused(), ""
		if t := pauses.pausedUntil(); paused && !t.IsZero() {
			until = t.Format(time.RFC3339)
		}
		util.WriteJSON(w, map[string]any{"paused": paused, "paused_until": until})
	}))

	// /ask and /events let the desktop window pose a question about what's on screen and stream the answer as it comes together — see internal/ipc for the route bodies.
	mux.HandleFunc("POST /ask", auth(ipcServer.Ask))
	mux.HandleFunc("GET /events", auth(ipcServer.Events))

	// A reply's link posts here instead of the webview's own window.open, which WebKitGTK does not reliably hand off to the system browser; the daemon opens it the same way the open_url tool does. See internal/ipc/open.go.
	mux.HandleFunc("POST /open", auth(ipc.Open(ipc.OpenCommand)))

	// A long computer-use goal: POST /act starts one and returns its id, GET /act/{id} is its whole record, and the four control routes stop it, hold it, carry it on and answer the one question a stuck job asks. Progress rides the same /events stream as an ask, tagged type "act" with the job's id.
	mux.HandleFunc("POST /act", auth(actJobs.Start))
	mux.HandleFunc("GET /act/{id}", auth(actJobs.Get))
	mux.HandleFunc("POST /act/{id}/stop", auth(actJobs.Stop))
	mux.HandleFunc("POST /act/{id}/pause", auth(actJobs.Pause))
	mux.HandleFunc("POST /act/{id}/resume", auth(actJobs.Resume))
	mux.HandleFunc("POST /act/{id}/answer", auth(actJobs.Answer))

	// Hold-to-talk dictation: /dictate/start opens the microphone, /dictate/stop transcribes what was said with the same local whisper.cpp build the meeting recorder uses and hands the text back for the window to put in its input. Route bodies are in internal/ipc/dictate.go.
	dictation := ipc.NewDictation(ipcServer)
	dictateStart, dictateStop := d.busy.trackDictation(dictation.Start, dictation.Stop)
	mux.HandleFunc("POST /dictate/start", auth(dictateStart))
	mux.HandleFunc("POST /dictate/stop", auth(dictateStop))

	// The window's read-only screens: what is on screen now, what is outstanding, what happened today, the meetings, a memory search and the people. Route bodies are in internal/ipc/reads.go.
	// While June's own window or the shell has the focus, /context names the window the tracker last saw the user in rather than the last one it captured, which is only filed after the dwell.
	ipcServer.SetLastWindow(daemon.LastWindow)
	mux.HandleFunc("/context", auth(ipcServer.Context))
	mux.HandleFunc("/matters", auth(ipcServer.Matters))
	mux.HandleFunc("/today", auth(ipcServer.Today))
	mux.HandleFunc("/meetings", auth(ipcServer.Meetings))
	mux.HandleFunc("/meetings/{id}", auth(ipcServer.Meeting)) // DELETE removes a meeting whose write-up is not worth keeping
	mux.HandleFunc("/meetings/live", auth(ipc.MeetingLive(meetingRecorder)))
	mux.HandleFunc("/memory/search", auth(ipcServer.MemorySearch))
	mux.HandleFunc("/people", auth(ipcServer.People))
	mux.HandleFunc("/people/{name}", auth(ipcServer.Person)) // DELETE takes a person out of personal context

	// The window's voice: the daemon runs the Gemini Live loop, and what the session hears, says and calls rides the /events stream above. Route bodies are in internal/ipc/voice.go.
	voiceSession := ipc.NewVoice(ipcServer, store, apiKey, actJobs.Spoken)
	d.busy.watchVoice(voiceSession)
	mux.HandleFunc("/voice/start", auth(voiceSession.Start))
	mux.HandleFunc("/voice/stop", auth(voiceSession.Stop))
	mux.HandleFunc("/voice/status", auth(voiceSession.Status))
	// Which voice that session speaks in, and hearing one before adopting it. The preview is a one-shot TTS call played through a speaker opened for it alone, so it works whether or not a session is running. Route bodies are in internal/ipc/voices.go.
	mux.HandleFunc("/voices", auth(ipc.Voices(liveConfig, nil)))
	mux.HandleFunc("/voices/preview", auth(ipc.VoicePreview(liveConfig, previewVoice(apiKey))))

	// The window's own record: the conversations it keeps and the turns inside them, the one list of work (action items plus the tasks the user typed in), the day pages, and which brains this machine is signed in to. Route bodies are in internal/ipc/conversations.go, tasks.go, days.go and brains.go.
	mux.HandleFunc("/conversations", auth(ipcServer.Conversations))
	mux.HandleFunc("/conversations/{id}", auth(ipcServer.Conversation)) // GET reads it, DELETE removes it
	mux.HandleFunc("/conversations/{id}/title", auth(ipcServer.ConversationTitle))
	mux.HandleFunc("/tasks", auth(ipcServer.Tasks))
	mux.HandleFunc("/tasks/{id}/done", auth(ipcServer.TaskDone))
	mux.HandleFunc("/tasks/{id}", auth(ipcServer.TaskOwner)) // PATCH corrects whose task it is, by hand
	// The window's own Done/1h/Evening/Tomorrow buttons on a live notice's rail line, answered through the same Act the desktop notification's own buttons call.
	mux.HandleFunc("POST /notices/{kind}/{id}/action", auth(ipc.NoticeAction(scheduler.Act)))

	mux.HandleFunc("/days", auth(ipcServer.Days))
	mux.HandleFunc("/days/{date}", auth(ipcServer.Day))
	mux.HandleFunc("/routines", auth(ipcServer.Routines))
	mux.HandleFunc("/routines/{id}", auth(ipcServer.RoutineDelete))
	mux.HandleFunc("/routines/{id}/run", auth(ipcServer.RoutineRun))
	// The model rosters are read once here, in the background, so the first settings render already has them: asking the command lines costs seconds and this route runs on every render.
	ipc.WarmModelCaches(nil)
	mux.HandleFunc("/brains", auth(ipc.Brains(liveConfig, brainLimits)))
	mux.HandleFunc("POST /overlay", auth(ipcServer.Overlay))
	// The shortcut that shows the hover, the same on both systems: GET /hotkey/check says whether one is free, POST /settings {"hotkey"} changes it, and POST /hotkey/status is the Windows window saying whether it could register one. Route bodies are in internal/ipc/hotkey.go.
	hotkeys := d.hotkeys
	hotkeys.Start()
	mux.HandleFunc("GET /hotkey/check", auth(hotkeys.CheckRoute))
	mux.HandleFunc("POST /hotkey/status", auth(hotkeys.StatusRoute))
	// The window's start-at-sign-in switch posts here too, and only cmd can change the login entry (see withAutostartSetting).
	mux.HandleFunc("/settings", auth(withAutostartSetting(liveConfig, ipc.Settings(config.DataDir(), liveConfig, appConfig.Meetings.OfferEnabled() || appConfig.Meetings.AutoRecord, daemon.IsPaused, startTime, brainLimits, hotkeys))))
	mux.HandleFunc("/usage", auth(ipc.Usage(store, appConfig.DailyTokenBudgetFor, appConfig.ExaMonthlyRequests, brainLimits)))

	// Quitting and restarting from the window and the installer (lifecycle.go), first-run setup, the local-features downloader and the updater. The last three report progress on the same /events stream as everything else.
	registerRoutes(mux, auth, lifecycleRoutes(d.restartBlocker))
	publish := func(typ, id, text, detail string, failed bool) {
		ipcServer.Publish(ipc.Event{ID: id, Type: typ, Text: text, Detail: detail, Failed: failed})
	}
	setup := ipc.SetupRoutes(ipc.SetupDeps{
		Config:  liveConfig,
		DataDir: config.DataDir(),
		Version: config.Version,
		// Finishing setup is the user asking for observation, so a pause left from before it is cleared with it.
		ResumeTracking: pauses.resume,
		Server:         ipcServer,
		RestartBlocker: d.restartBlocker,
		Hotkeys:        hotkeys,
	})
	// Every setup route but the read can restart June on the user's behalf, saving the key above all, and the window they came from expects to come back (see restartAsks).
	for pattern, h := range setup {
		if !strings.HasPrefix(pattern, http.MethodGet+" ") {
			setup[pattern] = countRestartAsk(h)
		}
	}
	registerRoutes(mux, auth, setup)
	registerRoutes(mux, auth, d.features.Routes())
	checker := update.New(update.Options{
		DataDir: config.DataDir(),
		ExeDir:  d.exeDir,
		Version: config.Version,
		Repo:    releaseRepo,
		Publish: publish,
		// The installer stops June with a quit, which is never refused, so an update is held back by the same things a restart is.
		Blocker: d.restartBlocker,
	})
	registerRoutes(mux, auth, checker.Routes())
	go checker.Run(d.ctx)
}

// releaseRepo is the GitHub repository June's releases are published from, which the updater reads the latest one of.
const releaseRepo = "M-DEV-1/june"

// brainLimitsFrom is the allowance lookup GET /brains and GET /usage draw their bars from. Input: the store the Codex and Claude readings land in, the Gemini daily request counter, the config accessor (for the Gemini model those requests are metered under, and for whether the Claude read is turned on, both read under its lock since POST /settings writes that flag from another request goroutine) and the configured ceilings. Output: a lookup taking a brain id and returning that brain's windows — Gemini's computed on the spot from the counter, Claude's read from its usage endpoint at most every ten minutes and only while someone is looking at the picker (skipped entirely when config.JuneConfig.ClaudeUsageFromLogin is off), Grok's read from its billing endpoint at most every ten minutes, Codex's whatever its last response's headers said after a login check at most every ten minutes, Antigravity's read in the background from agy's own /usage at most every ten minutes, and nothing at all for Ollama, which exposes no allowance to read.
func brainLimitsFrom(usage *brain.UsageStore, quota *brain.QuotaState, cfg *ipc.LiveConfig, opts brain.QuotaOptions) ipc.BrainLimits {
	return func(ctx context.Context, id string) (brain.UsageSnapshot, bool) {
		switch id {
		case "gemini":
			model, ok := brain.GeminiModelFor(cfg.Get().Brain)
			if !ok {
				model = config.TextModel
			}
			return brain.GeminiDaily(quota, model, opts, time.Now())
		case "claude":
			if cfg.ClaudeUsageEnabled() {
				agent.RefreshClaudeUsage(ctx)
			}
		case "grok":
			agent.RefreshGrokUsage(ctx)
		case "antigravity":
			// Read in the background with agy's own /usage, which spends no quota; this look draws the last reading and the next one draws the new one.
			agent.RefreshAgyUsage()
		case "codex":
			// Codex's allowance windows ride the headers of real calls, but whether the login still works has its own free check — the profile endpoint the Codex CLI's own /usage card reads. It spends nothing and rotates nothing, so the row can say the login is dead before a question is asked of it.
			agent.RefreshCodexLogin(ctx)
		}
		return usage.Get(id)
	}
}

// previewVoice builds the previewer POST /voices/preview speaks through. It opens a speaker for the one line and closes it again, so nothing is held open between previews and a machine with no working audio fails this one request rather than the daemon's startup. Input: the Gemini API key. Output: the previewer, or nil when this build has no speaker at all, which the route answers 503 for.
func previewVoice(apiKey string) ipc.VoicePreviewer {
	return func(ctx context.Context, name string) error {
		// Refused before the speaker is opened: with no key and nothing kept there is no line to play, and on a machine with no output device the missing key was reported as a missing speaker.
		if agent.PreviewNeedsKey(apiKey, name) {
			return agent.ErrNoGeminiKey
		}
		speaker, err := audio.NewSpeaker()
		if err != nil {
			return fmt.Errorf("no speaker to preview through: %w", err)
		}
		defer speaker.Close()
		if err := agent.SpeakPreview(ctx, apiKey, name, speaker.Play); err != nil {
			return err
		}
		// Play only queues the audio and Close drops whatever is still queued, so the line is waited out first or the preview is cut off. Flush used to stand here as that wait, but it throws the queue away rather than playing it, and the preview came out silent.
		d, ok := speaker.(audio.Drainer)
		if !ok {
			return fmt.Errorf("this speaker cannot wait for the voice preview to play, so it would be cut off")
		}
		waitCtx, cancel := context.WithTimeout(ctx, previewPlayLimit)
		defer cancel()
		if err := d.Drain(waitCtx); err != nil {
			return fmt.Errorf("the voice preview did not finish playing: %w", err)
		}
		return nil
	}
}

// previewPlayLimit bounds how long a voice preview waits for its line to play out. The line runs about two seconds, so a speaker that has not played it by then is not going to, and the request should fail rather than hang.
const previewPlayLimit = 15 * time.Second
