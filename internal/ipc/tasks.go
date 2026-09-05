// tasks.go holds GET/POST /tasks and POST /tasks/{id}/done: the one list of work the window shows, drawn from two places at once — the action items meetings raised, which live as notes of kind "action", and the tasks the user typed in themselves, which live in user_tasks.
package ipc

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ora/internal/memory"
)

// userTaskPrefix marks the ids of tasks the user typed in, so POST /tasks/{id}/done knows whether to close a user_tasks row or an action note. A noticed task's id is the plain note id, the same id /matters already hands out.
const userTaskPrefix = "task-"

// Task is one thing to do on GET /tasks. Source is "you" for a task the user typed in and "noticed" for an action item a meeting raised. When is when it was raised, Detail its provenance (the meeting or note title a noticed task was raised from, "" for one the user typed in), and ConversationID the conversation opened alongside it (empty for a noticed task).
type Task struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	Source         string `json:"source"`
	When           string `json:"when"`
	Done           bool   `json:"done"`
	ConversationID string `json:"conversation_id"`
	Detail         string `json:"detail"`
}

// Tasks handles /tasks: GET lists the open action items and every task the user typed in; POST creates one of the user's own from {"title"} and answers 201 with its id and the id of the conversation opened with it. Any other method is 405.
func (s *Server) Tasks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listTasks(w, r)
	case http.MethodPost:
		s.createTask(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// listTasks answers GET /tasks: the user's own tasks first, then the action items still owed, both newest first within their group.
func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tasks := []Task{}

	mine, err := s.store.UserTasks(ctx)
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	for _, t := range mine {
		conv := ""
		if t.ConversationID != 0 {
			conv = strconv.FormatInt(t.ConversationID, 10)
		}
		tasks = append(tasks, Task{
			ID:             userTaskPrefix + strconv.FormatInt(t.ID, 10),
			Title:          t.Title,
			Source:         "you",
			When:           rfc3339(t.Created),
			Done:           t.Done,
			ConversationID: conv,
			Detail:         "",
		})
	}

	// Read as notes rather than through OpenActionItems: the time the window shows is the note's own date, and only the note carries it — an item parsed out of minutes has a raised date only when the minutes named one.
	notes, err := s.store.NotesOfKindSince(ctx, memory.ActionNoteKind, time.Time{})
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	for _, n := range notes {
		a, ok := memory.ParseAction(n.Content)
		if !ok || a.Status != memory.StatusOpen || !a.Mine() {
			continue
		}
		tasks = append(tasks, Task{
			ID:     strconv.FormatInt(n.ID, 10),
			Title:  a.Text,
			Source: "noticed",
			When:   rfc3339(n.CreatedAt),
			Done:   false,
			Detail: a.Source,
		})
	}
	writeJSON(w, map[string]any{"tasks": tasks})
}

// createTask answers POST /tasks: a task of the user's own plus a conversation named after it, so asking Ora about the task has somewhere to go. A blank title is 400.
func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title string `json:"title"`
	}
	if !DecodeJSON(w, r, &req) {
		return
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		http.Error(w, "a task needs a title", http.StatusBadRequest)
		return
	}

	convID, err := s.store.CreateConversation(r.Context(), title, "")
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	id, err := s.store.AddUserTask(r.Context(), title, convID)
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{
		"id":              userTaskPrefix + strconv.FormatInt(id, 10),
		"conversation_id": strconv.FormatInt(convID, 10),
	})
}

// TaskDone handles POST /tasks/{id}/done with body {"done":bool} or {"status":"open"|"done"|"dropped"}. A "task-N" id flips the user's own task, which only ever carries a plain done/open state — "dropped" on one of these is 400, since user_tasks has no third state to hold it in. A plain note id sets that action item's status through the same store method the agent's revise tool calls, so a tick here, a drop here and a spoken correction all end in the same place; dropped items stop being returned by GET /tasks the same way done ones already do. An id that matches nothing is 404, a bad body or an unrecognised status 400.
func (s *Server) TaskDone(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Done   bool   `json:"done"`
		Status string `json:"status"`
	}
	if !DecodeJSON(w, r, &req) {
		return
	}
	status := req.Status
	if status == "" {
		status = memory.StatusOpen
		if req.Done {
			status = memory.StatusDone
		}
	} else if !memory.ValidStatus(status) {
		http.Error(w, "\""+status+"\" is not a task status", http.StatusBadRequest)
		return
	}

	raw := r.PathValue("id")
	if rest, ok := strings.CutPrefix(raw, userTaskPrefix); ok {
		if status == memory.StatusDropped {
			http.Error(w, "a task you typed in cannot be dropped", http.StatusBadRequest)
			return
		}
		id, err := strconv.ParseInt(rest, 10, 64)
		if err != nil {
			http.Error(w, "no such task", http.StatusNotFound)
			return
		}
		if err := s.store.SetUserTaskDone(r.Context(), id, status == memory.StatusDone); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	noteID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		http.Error(w, "no such task", http.StatusNotFound)
		return
	}
	if err := s.store.SetActionStatus(r.Context(), noteID, status); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
}
