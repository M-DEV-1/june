// routines.go holds GET/POST/DELETE /routines and POST /routines/{id}/run: user-authored scheduled instructions June checks on its own — "every weekday at 8, tell me the one thing I must do today" — and the one-click way to run one right now instead of waiting for its schedule. internal/proactive is what actually checks the schedules on a tick; this file is the window's read/write surface on the routines table, plus the immediate run, which asks through the same Asker /ask uses.
package ipc

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"june/internal/db"
	"june/internal/proactive"
	"june/internal/util"
)

// RoutineView is one routine on GET /routines. LastRun is "" until it has fired at least once. ScheduleError is "" for a schedule the scheduler can read, and otherwise says why it cannot: such a routine is skipped on every tick and will never run on its own, which nothing else on the screen would show.
type RoutineView struct {
	ID            string `json:"id"`
	Text          string `json:"text"`
	Schedule      string `json:"schedule"`
	Enabled       bool   `json:"enabled"`
	LastRun       string `json:"last_run"`
	LastAnswer    string `json:"last_answer"`
	ScheduleError string `json:"schedule_error"`
}

// viewRoutine converts a stored routine to the shape GET /routines answers with.
func viewRoutine(r db.Routine) RoutineView {
	lastRun := ""
	if !r.LastRun.IsZero() {
		lastRun = rfc3339(r.LastRun)
	}
	scheduleError := ""
	if _, err := proactive.ParseSchedule(r.Schedule); err != nil {
		scheduleError = err.Error()
	}
	return RoutineView{
		ID:            strconv.FormatInt(r.ID, 10),
		Text:          r.Text,
		Schedule:      r.Schedule,
		Enabled:       r.Enabled,
		LastRun:       lastRun,
		LastAnswer:    r.LastAnswer,
		ScheduleError: scheduleError,
	}
}

// scheduleForms is the line a refused schedule is answered with, naming the forms proactive.ParseSchedule reads.
const scheduleForms = `try "every day at 9am", "weekdays at 18:30", "every 4 hours" or "when <something happens>"`

// Routines handles /routines: GET lists every routine, newest first; POST creates one from {"text","schedule"}, both required. Any other method is 405.
func (s *Server) Routines(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listRoutines(w, r)
	case http.MethodPost:
		s.createRoutine(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// listRoutines answers GET /routines.
func (s *Server) listRoutines(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.Routines(r.Context())
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	out := make([]RoutineView, 0, len(rows))
	for _, row := range rows {
		out = append(out, viewRoutine(row))
	}
	util.WriteJSON(w, map[string]any{"routines": out})
}

// createRoutine answers POST /routines: a blank instruction or schedule is 400, and so is a schedule the scheduler cannot read, with the reason and the forms it can. Such a routine used to be stored and listed like any other and then skipped on every tick with nothing but a log line, so it simply never ran.
func (s *Server) createRoutine(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text     string `json:"text"`
		Schedule string `json:"schedule"`
	}
	if !DecodeJSON(w, r, &req) {
		return
	}
	text, schedule := strings.TrimSpace(req.Text), strings.TrimSpace(req.Schedule)
	if text == "" || schedule == "" {
		http.Error(w, "a routine needs both an instruction and a schedule", http.StatusBadRequest)
		return
	}
	if _, err := proactive.ParseSchedule(schedule); err != nil {
		http.Error(w, err.Error()+"; "+scheduleForms, http.StatusBadRequest)
		return
	}
	id, err := s.store.AddRoutine(r.Context(), text, schedule)
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"id": strconv.FormatInt(id, 10)})
}

// RoutineDelete handles DELETE /routines/{id}. Any other method is 405, an id that matches nothing is 404.
func (s *Server) RoutineDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "no such routine", http.StatusNotFound)
		return
	}
	if err := s.store.DeleteRoutine(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// RoutineRun handles POST /routines/{id}/run: it asks the routine's own instruction right now, through the same Asker /ask uses, records the answer as the routine's last run, and — unless the model answered exactly "NOTHING" — delivers it the same way a scheduled run would (see SetSay), so it falls back to a desktop notification when no window is listening. Answers 202 with {"id"} as soon as the run has started, because the ask can take minutes and the window's Run button must not spin for all of them; the result reaches the window as a notice, not as this response. An id that matches nothing is 404. A routine already running — this same route hit twice, or the scheduler's own tick landing on it first — answers 409 rather than asking and recording a second result. The ask runs on its own askTimeout-bounded context rather than the request's: closing the window that made this call must not silently cut off a minutes-long ask, the same reasoning as /ask's own run().
func (s *Server) RoutineRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "no such routine", http.StatusNotFound)
		return
	}
	routine, err := s.store.RoutineByID(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	// Claimed before the response is written, so a second click still gets its 409 rather than starting a second ask against the same routine.
	if !s.store.TryStart(id) {
		http.Error(w, "routine is already running", http.StatusConflict)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"id": strconv.FormatInt(id, 10)})

	go s.runRoutine(id, routine.Text)
}

// runRoutine asks one routine's instruction and files what it said, on its own goroutine and its own askTimeout-bounded context. Input: the routine's row id and its instruction. Output: none — the answer is recorded as the routine's last run and, unless it was exactly "NOTHING", delivered as a notice. A failure is logged rather than returned: the caller has already been answered, and the in-flight claim is released either way.
func (s *Server) runRoutine(id int64, text string) {
	defer s.store.Finish(id)

	ctx, cancel := context.WithTimeout(context.Background(), askTimeout)
	defer cancel()
	trace, err := s.asker.AskText(ctx, text+db.RoutineSuffix)
	if err != nil {
		slog.Error("routine: the run-now ask failed", "id", id, "error", err)
		return
	}
	answer := strings.TrimSpace(trace.Answer)
	if err := s.store.SetRoutineRun(ctx, id, time.Now(), answer); err != nil {
		slog.Error("routine: could not record the run", "id", id, "error", err)
		return
	}
	if answer != db.RoutineNothing {
		s.sayNotice(Notice{Title: "Routine", Body: answer, Place: "routine", ID: strconv.FormatInt(id, 10), Kind: "routine"})
	}
}
