// conversations.go holds the window's record of what was said: the list of conversations, one conversation with its turns, and the writes /ask makes into them. Same rules as reads.go — JSON out, no null lists, times RFC3339.
package ipc

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"ora/internal/db"
	"ora/internal/util"
)

// conversationsCap bounds the list the window's sidebar draws.
const conversationsCap = 50

// titleWords is how many of the first question's words become a conversation's title when nobody named it.
const titleWords = 8

// untitled is the title of a conversation opened with no title and no question to take one from.
const untitled = "New conversation"

// ConversationSummary is one row of GET /conversations: enough to draw the sidebar without loading any turns.
type ConversationSummary struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Brain   string `json:"brain"`
	Last    string `json:"last"`
	Updated string `json:"updated"`
}

// TurnView is one thing said in a conversation. Role is "you" or "ora"; Kind is "ask", "dictation", "voice" or "error"; Evidence and Tools are the supporting rows and the tool names behind an answer, both empty lists for a question. Reason is a plain-English summary of what went wrong, set only when Kind is "error" and "" otherwise.
type TurnView struct {
	ID       string         `json:"id"`
	Role     string         `json:"role"`
	Text     string         `json:"text"`
	Kind     string         `json:"kind"`
	Evidence []EvidenceItem `json:"evidence"`
	Tools    []string       `json:"tools"`
	When     string         `json:"when"`
	Reason   string         `json:"reason"`
}

// ConversationView is GET /conversations/{id}: one conversation and everything said in it, oldest first.
type ConversationView struct {
	ID    string     `json:"id"`
	Title string     `json:"title"`
	Brain string     `json:"brain"`
	Turns []TurnView `json:"turns"`
}

// Conversations handles /conversations: GET lists the 50 most recently touched conversations, newest first; POST opens one from an optional title and brain and answers 201 with its id. Any other method is 405.
func (s *Server) Conversations(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		convs, err := s.store.ListConversations(r.Context(), conversationsCap)
		if err != nil {
			fail(w, err, http.StatusInternalServerError)
			return
		}
		out := make([]ConversationSummary, 0, len(convs))
		for _, c := range convs {
			last := util.Runes(strings.TrimSpace(c.Last), maxEntryText)
			if c.LastKind == "error" {
				last = errorReason(c.Last)
			}
			out = append(out, ConversationSummary{
				ID:      strconv.FormatInt(c.ID, 10),
				Title:   c.Title,
				Brain:   c.Brain,
				Last:    last,
				Updated: rfc3339(c.Updated),
			})
		}
		writeJSON(w, map[string]any{"conversations": out})
	case http.MethodPost:
		var req struct {
			Title string `json:"title"`
			Brain string `json:"brain"`
		}
		// An empty body is an ordinary "open me a conversation", so a decode failure is only reported when there was something to decode.
		if !decodeJSONOptional(w, r, &req) {
			return
		}
		title := strings.TrimSpace(req.Title)
		if title == "" {
			title = untitled
		}
		id, err := s.store.CreateConversation(r.Context(), title, req.Brain)
		if err != nil {
			fail(w, err, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"id": strconv.FormatInt(id, 10)})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// Conversation handles /conversations/{id}: GET answers the conversation and its turns, oldest first; DELETE removes it and every turn said in it, answering 204. An id that is not a number, or names no conversation, is 404 either way — the window asking for or removing one that is not there is a mistake, not an empty page or a silent no-op.
func (s *Server) Conversation(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "no such conversation", http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.getConversation(w, r, id)
	case http.MethodDelete:
		if err := s.store.DeleteConversation(r.Context(), id); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// getConversation is GET /conversations/{id}'s body, split out from Conversation so the method switch above reads plainly.
func (s *Server) getConversation(w http.ResponseWriter, r *http.Request, id int64) {
	conv, err := s.store.Conversation(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	turns, err := s.store.ConversationTurns(r.Context(), id)
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, ConversationView{
		ID:    strconv.FormatInt(conv.ID, 10),
		Title: conv.Title,
		Brain: conv.Brain,
		Turns: turnViews(turns),
	})
}

// ConversationTitle handles POST /conversations/{id}/title with body {"title": string}: renames the conversation. Output: 204 on success; 400 for a body that fails to decode or names a blank title; 404 when no conversation has that id.
func (s *Server) ConversationTitle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "no such conversation", http.StatusNotFound)
		return
	}
	var req struct {
		Title string `json:"title"`
	}
	if !DecodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Title) == "" {
		http.Error(w, "a conversation needs a title", http.StatusBadRequest)
		return
	}
	if err := s.store.RenameConversation(r.Context(), id, req.Title); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// turnViews maps stored turns to what the window draws. Input: the turns, oldest first. Output: one view each, with evidence and tools as empty lists rather than null when the turn has none.
func turnViews(turns []db.Turn) []TurnView {
	out := make([]TurnView, 0, len(turns))
	for _, t := range turns {
		evidence := []EvidenceItem{}
		if len(t.Evidence) > 0 {
			json.Unmarshal(t.Evidence, &evidence) //nolint:errcheck — a mangled evidence column costs the citations, not the turn
			if evidence == nil {
				evidence = []EvidenceItem{}
			}
		}
		tools := t.Tools
		if tools == nil {
			tools = []string{}
		}
		reason := ""
		if t.Kind == "error" {
			reason = errorReason(t.Text)
		}
		out = append(out, TurnView{
			ID:       strconv.FormatInt(t.ID, 10),
			Role:     t.Role,
			Text:     t.Text,
			Kind:     t.Kind,
			Evidence: evidence,
			Tools:    tools,
			When:     rfc3339(t.When),
			Reason:   reason,
		})
	}
	return out
}

// errorReason turns a failed turn's stored error text into one plain sentence for the window's transcript. Input: the turn's text, exactly as it was stored when the ask failed (see run in ipc.go, which stores err.Error() verbatim under kind "error"). Output: one of four sentences, chosen by the same classification askSentence (askerror.go) uses for a live ask failure, so a transcript read back later and a failure shown at the moment it happened never disagree about what kind of error it was. There is no error value here, only the text that was stored, so askSentence is given a plain errors.New(text) — the context.DeadlineExceeded/context.Canceled cases it also checks never match that, which is fine because the text-based timeout/cancellation checks further down cover the same ground.
func errorReason(text string) string {
	switch askSentence(errors.New(text), text) {
	case askQuotaSpent, askTooFast:
		return "The model's quota is spent (429)."
	case askOverloaded:
		return "The model was overloaded (503)."
	case askTooLong:
		return "The model took too long."
	}
	// askSentence only reads these two out of a status code, but a stored error can carry the provider's status word with no numeric code beside it (the transcript keeps whatever text the failed call returned, unlike a live call's typed API error), so they are still checked directly here.
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "resource_exhausted"):
		return "The model's quota is spent (429)."
	case strings.Contains(lower, "unavailable"):
		return "The model was overloaded (503)."
	default:
		return "The model could not answer."
	}
}

// titleFromQuestion names a conversation after the question that opened it. Input: the first question. Output: its first eight words, or "New conversation" when the question is blank.
func titleFromQuestion(question string) string {
	words := strings.Fields(question)
	if len(words) == 0 {
		return untitled
	}
	if len(words) > titleWords {
		words = words[:titleWords]
	}
	return strings.Join(words, " ")
}
