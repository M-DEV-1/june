// This file is the storage behind the desktop window's own screens: the conversations it keeps (a title, the brain that answered, and the turns inside), the tasks the user types in themselves, and the list of days that have anything in them at all. Nothing here is memory — a turn is a record of what was said in the window, so none of these tables is indexed, searched or fed to a model. Action items are not here either: they stay notes of kind "action" (see action.go).
package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Conversation is one thread of question and answer in the window. Last is the text of its newest turn, empty for a conversation nobody has spoken in yet; LastKind is that turn's kind ("ask", "dictation", "voice" or "error"), also empty when there is no turn yet.
type Conversation struct {
	ID       int64
	Title    string
	Brain    string
	Last     string
	LastKind string
	Created  time.Time
	Updated  time.Time
}

// Turn is one thing said in a conversation. Role is "you" or "ora"; Kind is "ask", "dictation", "voice" or "error" (an answer that failed, filed so the thread never shows a question with nothing under it); Evidence is the JSON array of supporting rows behind an answer, nil when there is none; Tools names the tools the agent called for that answer.
type Turn struct {
	ID             int64
	ConversationID int64
	Role           string
	Text           string
	Kind           string
	Evidence       json.RawMessage
	Tools          []string
	When           time.Time
}

// UserTask is a task the user typed in themselves, as against an action item a meeting raised. ConversationID is the conversation opened alongside it, 0 when none was.
type UserTask struct {
	ID             int64
	Title          string
	Done           bool
	ConversationID int64
	Created        time.Time
}

// CreateConversation opens a new conversation. Input: its title and the brain that will answer in it, both allowed to be empty. Output: the new conversation's id, or an error from the store.
func (s *Store) CreateConversation(ctx context.Context, title, brain string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO conversations (title, brain) VALUES (?, ?)`, strings.TrimSpace(title), strings.TrimSpace(brain))
	if err != nil {
		return 0, fmt.Errorf("create conversation: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("create conversation: %w", err)
	}
	return id, nil
}

// ListConversations returns the most recently touched conversations, newest first. Input: how many to return. Output: each conversation with the text and kind of its newest turn as Last and LastKind.
func (s *Store) ListConversations(ctx context.Context, limit int) ([]Conversation, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.title, c.brain, c.created_at, c.updated_at,
			IFNULL((SELECT t.text FROM conversation_turns t WHERE t.conversation_id = c.id ORDER BY t.id DESC LIMIT 1), ''),
			IFNULL((SELECT t.kind FROM conversation_turns t WHERE t.conversation_id = c.id ORDER BY t.id DESC LIMIT 1), '')
		FROM conversations c
		ORDER BY c.updated_at DESC, c.id DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}
	defer rows.Close()

	var out []Conversation
	for rows.Next() {
		var c Conversation
		if err := rows.Scan(&c.ID, &c.Title, &c.Brain, &c.Created, &c.Updated, &c.Last, &c.LastKind); err != nil {
			return nil, fmt.Errorf("list conversations: scan: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// RenameConversation changes a conversation's title. Input: its id and the new title (trimmed before storing). Output: an error when the title is blank or no conversation has that id — a rename the store never took must not be reported as done.
func (s *Store) RenameConversation(ctx context.Context, id int64, title string) error {
	title = strings.TrimSpace(title)
	if title == "" {
		return fmt.Errorf("a conversation needs a title")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE conversations SET title = ? WHERE id = ?`, title, id)
	if err != nil {
		return fmt.Errorf("rename conversation: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rename conversation: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("no conversation with id %d", id)
	}
	return nil
}

// Conversation returns one conversation by id. Output: the conversation without its turns (see ConversationTurns), or an error when no such conversation exists — the window asking for one that is not there is a mistake worth reporting, not an empty page.
func (s *Store) Conversation(ctx context.Context, id int64) (Conversation, error) {
	var c Conversation
	err := s.db.QueryRowContext(ctx,
		`SELECT id, title, brain, created_at, updated_at FROM conversations WHERE id = ?`, id).
		Scan(&c.ID, &c.Title, &c.Brain, &c.Created, &c.Updated)
	if err == sql.ErrNoRows {
		return Conversation{}, fmt.Errorf("no conversation with id %d", id)
	}
	if err != nil {
		return Conversation{}, fmt.Errorf("read conversation: %w", err)
	}
	return c, nil
}

// ConversationTurns returns everything said in one conversation, oldest first.
func (s *Store) ConversationTurns(ctx context.Context, id int64) ([]Turn, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, conversation_id, role, text, kind, evidence, tools, created_at FROM conversation_turns WHERE conversation_id = ? ORDER BY id ASC`, id)
	if err != nil {
		return nil, fmt.Errorf("conversation turns: %w", err)
	}
	return scanTurns(rows)
}

// TurnsBetween returns every turn of every conversation whose time falls in [from, to], oldest first — the day page's record of what the user asked that day.
func (s *Store) TurnsBetween(ctx context.Context, from, to time.Time) ([]Turn, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, conversation_id, role, text, kind, evidence, tools, created_at FROM conversation_turns WHERE created_at >= ? AND created_at <= ? ORDER BY id ASC`,
		sqliteUTC(from), sqliteUTC(to))
	if err != nil {
		return nil, fmt.Errorf("turns between: %w", err)
	}
	return scanTurns(rows)
}

// scanTurns reads a query of conversation_turns into Turns, decoding the stored JSON columns. A tools column that no longer parses leaves the names empty rather than failing the read: the text of what was said matters more than the list of tools behind it.
func scanTurns(rows *sql.Rows) ([]Turn, error) {
	defer rows.Close()
	var out []Turn
	for rows.Next() {
		var t Turn
		var evidence, tools string
		if err := rows.Scan(&t.ID, &t.ConversationID, &t.Role, &t.Text, &t.Kind, &evidence, &tools, &t.When); err != nil {
			return nil, fmt.Errorf("scan turn: %w", err)
		}
		if strings.TrimSpace(evidence) != "" {
			t.Evidence = json.RawMessage(evidence)
		}
		if strings.TrimSpace(tools) != "" {
			json.Unmarshal([]byte(tools), &t.Tools) //nolint:errcheck — a mangled tools column costs the tool names, not the turn
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// AddTurn records one thing said in a conversation and marks the conversation as touched. Input: the conversation, the role ("you" or "ora"), the text, the kind ("ask", "dictation", "voice" or "error"), the evidence JSON behind an answer (nil when there is none) and the tool names behind it. Output: the new turn's id, or an error when no conversation has that id.
// The two writes go in one transaction and the touch's row count is what checks the conversation exists. conversation_turns' foreign key on conversation_id is enforced: PRAGMA foreign_keys(1) rides in the store's DSN, so the driver replays it on every connection it opens and SQLite itself refuses an insert against an id that names no conversation. The row-count check is kept in front of that key because it gives the better error — "no conversation with id 42" instead of the driver's bare FOREIGN KEY constraint failed — and because it does not depend on how the connection happened to be opened. Same reasoning as SetUserTaskDone below — a write the store never really took must not be reported as taken.
func (s *Store) AddTurn(ctx context.Context, conversationID int64, role, text, kind string, evidence json.RawMessage, tools []string) (int64, error) {
	toolsJSON := ""
	if len(tools) > 0 {
		b, err := json.Marshal(tools)
		if err != nil {
			return 0, fmt.Errorf("add turn: %w", err)
		}
		toolsJSON = string(b)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("add turn: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck — no-op after a successful Commit

	res, err := tx.ExecContext(ctx,
		`INSERT INTO conversation_turns (conversation_id, role, text, kind, evidence, tools) VALUES (?, ?, ?, ?, ?, ?)`,
		conversationID, role, text, kind, string(evidence), toolsJSON)
	if err != nil {
		return 0, fmt.Errorf("add turn: %w", err)
	}
	touched, err := tx.ExecContext(ctx, `UPDATE conversations SET updated_at = CURRENT_TIMESTAMP WHERE id = ?`, conversationID)
	if err != nil {
		return 0, fmt.Errorf("add turn: touch conversation: %w", err)
	}
	n, err := touched.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("add turn: touch conversation: %w", err)
	}
	if n == 0 {
		return 0, fmt.Errorf("no conversation with id %d", conversationID)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("add turn: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("add turn: commit: %w", err)
	}
	return id, nil
}

// AddUserTask stores a task the user typed in. Input: its title and the conversation opened alongside it (0 for none). Output: the new task's id.
func (s *Store) AddUserTask(ctx context.Context, title string, conversationID int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO user_tasks (title, conversation_id) VALUES (?, ?)`, strings.TrimSpace(title), conversationID)
	if err != nil {
		return 0, fmt.Errorf("add user task: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("add user task: %w", err)
	}
	return id, nil
}

// UserTasks returns every task the user typed in, newest first, done ones included — the window shows a closed task struck through rather than hiding it.
func (s *Store) UserTasks(ctx context.Context) ([]UserTask, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, title, done, conversation_id, created_at FROM user_tasks ORDER BY id DESC`)
	if err != nil {
		return nil, fmt.Errorf("user tasks: %w", err)
	}
	defer rows.Close()

	var out []UserTask
	for rows.Next() {
		var t UserTask
		if err := rows.Scan(&t.ID, &t.Title, &t.Done, &t.ConversationID, &t.Created); err != nil {
			return nil, fmt.Errorf("scan user task: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetUserTaskTitle rewrites one of the user's own tasks. Input: the task's id and its new title. Output: an error when nothing matched the id, on the same rule SetUserTaskDone holds to.
//
// A task could be created and ticked and nothing else until 2026-09-12, when the user asked three times to put the right context on one Ora had just made for him and was refused every time. A task whose words cannot be corrected is a task that has to be made again from scratch.
func (s *Store) SetUserTaskTitle(ctx context.Context, id int64, title string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE user_tasks SET title = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, strings.TrimSpace(title), id)
	if err != nil {
		return fmt.Errorf("set user task title: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set user task title: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("no task with id %d", id)
	}
	return nil
}

// DeleteUserTask removes one of the user's own tasks outright. Input: the task's id. Output: an error when nothing matched it.
//
// Ticking a task done is not the same as never having wanted it: a task Ora added by mistake, or one the user asks it to get rid of, has to go rather than sit on the list struck through. Nothing in the product could remove one before 2026-09-12.
func (s *Store) DeleteUserTask(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM user_tasks WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete user task: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete user task: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("no task with id %d", id)
	}
	return nil
}

// SetUserTaskDone marks one of the user's own tasks done or open again. An id that matches nothing is an error, not a silent no-op — reporting a tick the store never took would lose it.
func (s *Store) SetUserTaskDone(ctx context.Context, id int64, done bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE user_tasks SET done = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, done, id)
	if err != nil {
		return fmt.Errorf("set user task done: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set user task done: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("no task with id %d", id)
	}
	return nil
}

// ActiveDays returns the local calendar days at or after since that have anything in them — a capture, a meeting, or a diary entry Ora wrote — newest day first, as 'YYYY-MM-DD' strings. Episode and note times are stored in UTC and converted to local here; diary days are already local.
func (s *Store) ActiveDays(ctx context.Context, since time.Time) ([]string, error) {
	sinceDay := since.Format("2006-01-02")
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT day FROM (
			SELECT date(created_at, 'localtime') AS day FROM episodes WHERE created_at >= ?
			UNION
			SELECT date(created_at, 'localtime') AS day FROM notes WHERE kind = 'meeting' AND created_at >= ?
			UNION
			SELECT day FROM diary WHERE kind = 'day'
		)
		WHERE day >= ?
		ORDER BY day DESC`, sqliteUTC(since), sqliteUTC(since), sinceDay)
	if err != nil {
		return nil, fmt.Errorf("active days: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var day string
		if err := rows.Scan(&day); err != nil {
			return nil, fmt.Errorf("scan active day: %w", err)
		}
		out = append(out, day)
	}
	return out, rows.Err()
}

// DeleteConversation removes a conversation and every turn said in it. Output: an error when no conversation has that id — a delete the store never took must not be reported as done.
func (s *Store) DeleteConversation(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete conversation: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck — no-op after a successful Commit

	if _, err := tx.ExecContext(ctx, `DELETE FROM conversation_turns WHERE conversation_id = ?`, id); err != nil {
		return fmt.Errorf("delete conversation: turns: %w", err)
	}
	// user_tasks.conversation_id carries no foreign key, so nothing in the database unlinks a task from the thread it was opened alongside. Left pointing at a deleted conversation, the task opens an empty thread in the window; zero is what "no conversation" already means there.
	if _, err := tx.ExecContext(ctx, `UPDATE user_tasks SET conversation_id = 0 WHERE conversation_id = ?`, id); err != nil {
		return fmt.Errorf("delete conversation: unlink its tasks: %w", err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM conversations WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete conversation: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete conversation: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("no conversation with id %d", id)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete conversation: commit: %w", err)
	}
	return nil
}

// EpisodeCountsByDay returns how many episodes were recorded on each local calendar day in [since, until] — the day list's "seen" count. Output: day ('YYYY-MM-DD') to count, with no entry for a day that had none.
func (s *Store) EpisodeCountsByDay(ctx context.Context, since, until time.Time) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT date(created_at, 'localtime') AS day, COUNT(*)
		FROM episodes
		WHERE created_at >= ? AND created_at <= ?
		GROUP BY day`, sqliteUTC(since), sqliteUTC(until))
	if err != nil {
		return nil, fmt.Errorf("episode counts by day: %w", err)
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var day string
		var n int
		if err := rows.Scan(&day, &n); err != nil {
			return nil, fmt.Errorf("scan episode count by day: %w", err)
		}
		out[day] = n
	}
	return out, rows.Err()
}
