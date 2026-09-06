package cmd

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"ora/internal/agent"
	"ora/internal/brain"
	"ora/internal/config"
	"ora/internal/db"
	"ora/internal/embed"
	"ora/internal/ipc"
	"ora/internal/memory"
	"ora/internal/proactive"
	"ora/internal/recorder"
	"ora/internal/tracker"
	"ora/internal/vector"
)

type routeDependencies struct {
	auth            func(http.HandlerFunc) http.HandlerFunc
	compiler        *memory.Compiler
	daemon          *tracker.Daemon
	vecIndex        *vector.ChromemIndex
	embedEngine     *embed.Engine
	ipcServer       *ipc.Server
	actJobs         *ipc.ActJobs
	store           *db.Store
	apiKey          string
	meetingRecorder *recorder.Recorder
	scheduler       *proactive.Scheduler
	liveConfig      *ipc.LiveConfig
	brainLimits     ipc.BrainLimits
	appConfig       config.OraConfig
	startTime       time.Time
}

// registerDaemonRoutes wires up the HTTP routes on mux for IPC communication.
func registerDaemonRoutes(mux *http.ServeMux, d routeDependencies) {
	auth := d.auth
	compiler := d.compiler
	daemon := d.daemon
	vecIndex := d.vecIndex
	embedEngine := d.embedEngine
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

	// data sharing endpoint, get latest tracking data
	mux.HandleFunc("/buffer", auth(func(w http.ResponseWriter, r *http.Request) {
		if compiler == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		buf := compiler.GetCurrentBuffer()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(buf)
	}))

	// The tray asks the window it started to open or to show its hover; the instruction travels on the event stream the window already reads.
	mux.HandleFunc("/window", auth(ipcServer.Window))

	// pause/resume tracking
	mux.HandleFunc("/pause", auth(func(w http.ResponseWriter, r *http.Request) {
		daemon.Pause()
		slog.Info("tracking paused via IPC")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("paused"))
	}))

	mux.HandleFunc("/resume", auth(func(w http.ResponseWriter, r *http.Request) {
		daemon.Resume()
		slog.Info("tracking resumed via IPC")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("resumed"))
	}))

	mux.HandleFunc("/status", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		paused := daemon.IsPaused()
		json.NewEncoder(w).Encode(map[string]bool{"paused": paused})
	}))

	// /vector/* let the client process reach the daemon's vector index over IPC instead of opening chromem itself — two processes opening the same chromem dir risks torn reads/corruption (see vecIndex's own doc comment above). All three return 503 with no body if the daemon has no vector index wired (no API key, or init failed) — the client's httpVectorIndex adapter treats any non-200 as an error, which HybridSearch already degrades gracefully from (see internal/db/hybrid.go's resilience handling).
	mux.HandleFunc("/vector/search", auth(func(w http.ResponseWriter, r *http.Request) {
		if vecIndex == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var req struct {
			Embedding []float32         `json:"embedding"`
			N         int               `json:"n"`
			Where     map[string]string `json:"where"`
		}
		if !ipc.DecodeJSON(w, r, &req) {
			return
		}
		results, err := vecIndex.Search(r.Context(), req.Embedding, req.N, req.Where)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"results": results})
	}))

	mux.HandleFunc("/vector/add", auth(func(w http.ResponseWriter, r *http.Request) {
		if vecIndex == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var req struct {
			ID        string            `json:"id"`
			Content   string            `json:"content"`
			Embedding []float32         `json:"embedding"`
			Metadata  map[string]string `json:"metadata"`
		}
		if !ipc.DecodeJSON(w, r, &req) {
			return
		}
		if err := vecIndex.Add(r.Context(), req.ID, req.Content, req.Embedding, req.Metadata); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	mux.HandleFunc("/vector/delete", auth(func(w http.ResponseWriter, r *http.Request) {
		if vecIndex == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var req struct {
			ID string `json:"id"`
		}
		if !ipc.DecodeJSON(w, r, &req) {
			return
		}
		if err := vecIndex.Delete(r.Context(), req.ID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	// /embed lets the client reach the daemon's embedding engine instead of running one of its own. Only the daemon may own the llama-server child (one process, one port), so this is the client's only route to a local vector. 503 with no body when the daemon is on the Gemini path or has no embedder at all, which the client's httpEmbedder reports as an error and HybridSearch degrades from.
	mux.HandleFunc("/embed", auth(func(w http.ResponseWriter, r *http.Request) {
		if embedEngine == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var req struct {
			Task string `json:"task"`
			Text string `json:"text"`
		}
		if !ipc.DecodeJSON(w, r, &req) {
			return
		}
		vec, err := embedEngine.Embed(r.Context(), embed.TaskType(req.Task), req.Text)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"embedding": vec})
	}))

	// /ask and /events let the desktop window pose a question about what's on screen and stream the answer as it comes together — see internal/ipc for the route bodies.
	mux.HandleFunc("/ask", auth(ipcServer.Ask))
	mux.HandleFunc("/events", auth(ipcServer.Events))

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
	mux.HandleFunc("/dictate/start", auth(dictation.Start))
	mux.HandleFunc("/dictate/stop", auth(dictation.Stop))

	// The window's read-only screens: what is on screen now, what is outstanding, what happened today, the meetings, a memory search and the people. Route bodies are in internal/ipc/reads.go.
	mux.HandleFunc("/context", auth(ipcServer.Context))
	mux.HandleFunc("/matters", auth(ipcServer.Matters))
	mux.HandleFunc("/today", auth(ipcServer.Today))
	mux.HandleFunc("/meetings", auth(ipcServer.Meetings))
	mux.HandleFunc("/meetings/live", auth(ipc.MeetingLive(meetingRecorder)))
	mux.HandleFunc("/memory/search", auth(ipcServer.MemorySearch))
	mux.HandleFunc("/people", auth(ipcServer.People))

	// The window's voice: the daemon runs the same Gemini Live loop the terminal client does, and what the session hears, says and calls rides the /events stream above. Route bodies are in internal/ipc/voice.go.
	voiceSession := ipc.NewVoice(ipcServer, store, apiKey)
	mux.HandleFunc("/voice/start", auth(voiceSession.Start))
	mux.HandleFunc("/voice/stop", auth(voiceSession.Stop))
	mux.HandleFunc("/voice/status", auth(voiceSession.Status))

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
	mux.HandleFunc("/brains", auth(ipc.Brains(liveConfig, brainLimits)))
	mux.HandleFunc("/overlay", auth(ipcServer.Overlay))
	mux.HandleFunc("/settings", auth(ipc.Settings(config.DataDir(), liveConfig, appConfig.Meetings.OfferEnabled() || appConfig.Meetings.AutoRecord, daemon.IsPaused, startTime)))
	mux.HandleFunc("/usage", auth(ipc.Usage(store, appConfig.DailyTokenBudgetFor, brainLimits)))
}

// brainLimitsFrom is the allowance lookup GET /brains and GET /usage draw their bars from. Input: the store the Codex and Claude readings land in, the Gemini daily request counter, the config accessor (for the Gemini model those requests are metered under, and for whether the Claude read is turned on, both read under its lock since POST /settings writes that flag from another request goroutine) and the configured ceilings. Output: a lookup taking a brain id and returning that brain's windows — Gemini's computed on the spot from the counter, Claude's read from its usage endpoint at most every ten minutes and only while someone is looking at the picker (skipped entirely when config.OraConfig.ClaudeUsageFromLogin is off), Codex's whatever its last response's headers said, and nothing at all for Grok and Ollama, which expose no allowance to read.
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
		}
		return usage.Get(id)
	}
}
