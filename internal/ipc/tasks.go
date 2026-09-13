// tasks.go holds GET/POST /tasks and POST /tasks/{id}/done: the one list of work the window shows, drawn from two places at once — the action items meetings raised, which live as notes of kind "action", and the tasks the user typed in themselves, which live in user_tasks.
package ipc

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"ora/internal/memory"
)

// allOwners is the ?owner value that filters nothing out.
const allOwners = "all"

// typedTaskDetail is where a task the user typed in came from, in the same slot a noticed task names its meeting.
const typedTaskDetail = "you said"

// raisedIn names where a noticed task came from: the meeting it was raised in and the day it was raised on, or just the day when the minutes never named the meeting.
func raisedIn(a memory.ActionItem) string {
	day := ""
	if !a.Raised.IsZero() {
		day = a.Raised.Format("2006-01-02")
	} else if !a.Created.IsZero() {
		day = a.Created.Local().Format("2006-01-02")
	}
	switch {
	case a.Source == "":
		return day
	case day == "":
		return a.Source
	}
	return a.Source + ", " + day
}

// userTaskPrefix marks the ids of tasks the user typed in, so POST /tasks/{id}/done knows whether to close a user_tasks row or an action note. A noticed task's id is the plain note id, the same id /matters already hands out.
const userTaskPrefix = "task-"

// Task is one thing to do on GET /tasks. Source is "you" for a task the user typed in and "noticed" for an action item a meeting raised. When is when it was raised, Detail where it came from (the meeting and the date it was raised on, or "you said" for one the user typed in), and ConversationID the conversation opened alongside it (empty for a noticed task).
// Owner is whose task it is — "me", "them" or "unclear" — which is what tells the user's own work from something he merely heard somebody else agree to.
type Task struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	Source         string `json:"source"`
	Owner          string `json:"owner"`
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
// ?owner=me is the default and is his work alone. ?owner=them is what he is waiting on other people for, ?owner=unclear the items a meeting left unowned, and ?owner=all the lot. The tasks he typed in are his by definition, so they appear under "me" and "all" and nowhere else.
func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tasks := []Task{}

	owner := r.URL.Query().Get("owner")
	if owner == "" {
		owner = memory.OwnerMe
	}
	if owner != memory.OwnerMe && owner != memory.OwnerThem && owner != memory.OwnerUnclear && owner != allOwners {
		http.Error(w, "owner must be me, them, unclear or all", http.StatusBadRequest)
		return
	}

	mine, err := s.store.UserTasks(ctx)
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	if owner != memory.OwnerMe && owner != allOwners {
		mine = nil
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
			Owner:          memory.OwnerMe,
			When:           rfc3339(t.Created),
			Done:           t.Done,
			ConversationID: conv,
			Detail:         typedTaskDetail,
		})
	}

	items, err := s.store.ActionItemsByOwner(ctx, owner)
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	identity := s.store.Identity(ctx)
	for _, a := range items {
		if a.Status != memory.StatusOpen {
			continue
		}
		tasks = append(tasks, Task{
			ID:     strconv.FormatInt(a.NoteID, 10),
			Title:  a.Text,
			Source: "noticed",
			Owner:  a.OwnerClass(identity),
			// The time the window shows is the note's own date, not the item's raised date: an item parsed out of minutes has a raised date only when the minutes named one.
			When:   rfc3339(a.Created),
			Done:   false,
			Detail: raisedIn(a),
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

// TaskOwner handles PATCH /tasks/{id} with body {"owner":"me"|"them"|"unclear"}: the user saying by hand whose task this really is, overriding whatever the meeting's minutes read as. Only a noticed task carries an owner to correct — a task the user typed in is always his by definition, so a "task-N" id is 400. A bad owner value is 400, and an id that names no action item is 404.
func (s *Server) TaskOwner(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		s.taskDelete(w, r)
		return
	}
	if r.Method != http.MethodPatch {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Owner string `json:"owner"`
	}
	if !DecodeJSON(w, r, &req) {
		return
	}
	if !memory.ValidOwnerClass(req.Owner) {
		http.Error(w, "owner must be me, them or unclear", http.StatusBadRequest)
		return
	}

	raw := r.PathValue("id")
	if strings.HasPrefix(raw, userTaskPrefix) {
		http.Error(w, "a task you typed in is always yours", http.StatusBadRequest)
		return
	}
	noteID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		http.Error(w, "no such task", http.StatusNotFound)
		return
	}
	if err := s.store.SetOwnerClass(r.Context(), noteID, req.Owner); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// taskDelete serves DELETE /tasks/{id}: it takes one of the user's own tasks off the list for good.
// Only a task the user typed in, or Ora put there for them, can be deleted. A noticed item is a note in memory carrying a "[state/priority]" prefix, and deleting it would take a line out of a meeting's minutes rather than off a list; dropping it is what that is for, through POST /tasks/{id}/done with status "dropped".
// Input: the id from the path, in the "task-N" form GET /tasks hands out. Output: 204 and no body; 400 for a noticed item, 404 for an id that names no task of the user's own.
func (s *Server) taskDelete(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(r.PathValue("id"), userTaskPrefix)
	if !ok {
		http.Error(w, "an item Ora noticed is dropped rather than deleted", http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		http.Error(w, "no such task", http.StatusNotFound)
		return
	}
	if err := s.store.DeleteUserTask(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
