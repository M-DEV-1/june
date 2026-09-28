// toolrecord.go files the agent's tool calls in the store. It is the daemon's half of agent.ToolRecorder: the agent states what it ran and how it ended, and this decides where that goes.
package ipc

import (
	"context"
	"log/slog"
	"time"

	"june/internal/agent"
	"june/internal/db"
)

// toolRecordTimeout bounds one row's write. The ask that produced the record has usually moved on by the time it lands, so the write cannot borrow the ask's context — it would be cancelled the moment the answer was spoken — and it cannot wait for ever either.
const toolRecordTimeout = 5 * time.Second

// toolRecorder builds the recorder one loop's tool calls are filed under. Input: the store (nil files nothing), the name of the loop running them — "ask", "voice", "act" or "subtask" — and the conversation the calls belong to, 0 when they belong to none. Output: the recorder to hang on the ask's context, or nil when there is no store to write to.
// A failed write is logged and swallowed: the record exists to be read later, and losing a row is a smaller harm than failing the tool call that produced it in front of the user.
func toolRecorder(store *db.Store, path string, convID int64) agent.ToolRecorder {
	if store == nil {
		return nil
	}
	return func(r agent.ToolRecord) {
		ctx, cancel := context.WithTimeout(context.Background(), toolRecordTimeout)
		defer cancel()
		if _, err := store.AddToolCall(ctx, db.ToolCall{
			Path:           path,
			ConversationID: convID,
			Name:           r.Name,
			Args:           r.Args,
			Outcome:        r.Outcome,
			Result:         r.Result,
			Output:         r.Output,
			TurnID:         r.TurnID,
			DurationMS:     r.Duration.Milliseconds(),
			Offered:        r.Offered,
		}); err != nil {
			slog.Warn("could not file a tool call", "tool", r.Name, "path", path, "error", err)
		}
	}
}
