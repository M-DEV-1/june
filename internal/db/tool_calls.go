// tool_calls.go is the record of what the model actually reached for: one row per tool call, from every path that runs one — a chat ask, a computer-use job, a background sub-task, and the live voice session, which until now recorded nothing at all.
// It exists so a later pass can tell three things apart that used to look identical in the store: a tool that was never offered, one that was offered and not chosen, and one that was chosen and refused.
package db

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ToolCall is one call the model made. Path says which loop ran it ("ask", "act", "voice", "subtask"). ConversationID ties it to the turn it belongs to, and is 0 for a call made outside any conversation. Args and Result are the same short summaries the activity feed shows, never the raw text: a screen listing is over a thousand tokens and has no business being kept twice. Outcome classes the result. Offered is every tool name the model could have called on that round, so an absence in this table can be read.
type ToolCall struct {
	ID             int64
	At             time.Time
	Path           string
	ConversationID int64
	Name           string
	Args           string
	Outcome        string
	Result         string
	DurationMS     int64
	Offered        []string
}

// AddToolCall files one call. Input: the call. Output: its id, or an error when the write failed.
func (s *Store) AddToolCall(ctx context.Context, c ToolCall) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO tool_calls (path, conversation_id, name, args, outcome, result, duration_ms, offered) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Path, c.ConversationID, c.Name, c.Args, c.Outcome, c.Result, c.DurationMS, strings.Join(c.Offered, ","))
	if err != nil {
		return 0, fmt.Errorf("add tool call: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("add tool call: %w", err)
	}
	return id, nil
}

// ToolCallsSince lists the calls filed at or after since, oldest first. Input: the moment to read from. Output: the calls, which is what the nightly pass reads.
func (s *Store) ToolCallsSince(ctx context.Context, since time.Time) ([]ToolCall, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, created_at, path, conversation_id, name, args, outcome, result, duration_ms, offered
		 FROM tool_calls WHERE created_at >= ? ORDER BY id`, sqliteUTC(since))
	if err != nil {
		return nil, fmt.Errorf("read tool calls: %w", err)
	}
	defer rows.Close()
	var out []ToolCall
	for rows.Next() {
		var c ToolCall
		var offered string
		if err := rows.Scan(&c.ID, &c.At, &c.Path, &c.ConversationID, &c.Name, &c.Args, &c.Outcome, &c.Result, &c.DurationMS, &offered); err != nil {
			return nil, fmt.Errorf("scan tool call: %w", err)
		}
		if offered != "" {
			c.Offered = strings.Split(offered, ",")
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
