package agent

import (
	"context"
	"errors"
	"ora/internal/db"
	"strings"
	"testing"
	"time"
)

func TestPersonalContextTool_SetViewDelete(t *testing.T) {
	ctx := context.Background()
	brain := &toolTestBrain{}
	a := &Agent{brain: brain}

	if got := a.executeTool(ctx, "personal_context", map[string]any{
		"action": "set", "subject": "vexil-quorin", "content": "Vexil Quorin is the user's colleague at Acme.",
	}); strings.HasPrefix(got, "error:") {
		t.Fatalf("set failed: %s", got)
	}
	if brain.personal["vexil-quorin"] != "Vexil Quorin is the user's colleague at Acme." {
		t.Errorf("set wrote %q", brain.personal["vexil-quorin"])
	}

	view := a.executeTool(ctx, "personal_context", map[string]any{"action": "view"})
	if !strings.Contains(view, "vexil-quorin") || !strings.Contains(view, "colleague at Acme") {
		t.Errorf("view didn't show the entry:\n%s", view)
	}

	if got := a.executeTool(ctx, "personal_context", map[string]any{"action": "delete", "subject": "vexil-quorin"}); strings.HasPrefix(got, "error:") {
		t.Fatalf("delete failed: %s", got)
	}
	if _, still := brain.personal["vexil-quorin"]; still {
		t.Error("delete left the entry behind")
	}
}

func TestPersonalContextTool_ReportsWriteFailures(t *testing.T) {
	ctx := context.Background()
	a := &Agent{brain: &toolTestBrain{personalErr: errors.New("disk on fire")}}

	got := a.executeTool(ctx, "personal_context", map[string]any{"action": "set", "subject": "identity", "content": "x"})
	if !strings.HasPrefix(got, "error:") {
		t.Errorf("a failed write reported success: %q", got)
	}
	if strings.Contains(got, "disk on fire") {
		t.Errorf("the raw store error reached the model: %q", got)
	}
}

// TestSystemInstruction_PersonalContextBlock checks the entries reach the prompt verbatim, under their own heading, and that the prompt points corrections at the personal_context tool — and that an empty store produces no block at all, keeping the prompt from carrying an empty heading on a fresh install.
func TestSystemInstruction_PersonalContextBlock(t *testing.T) {
	if block := personalContextBlock(nil); block != "" {
		t.Errorf("an empty store produced a block: %q", block)
	}
	empty := systemInstructionText(time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC), "linux", "amd64", "sh", "", "some context", 5)
	if strings.Contains(empty, "Personal context — things known for certain") {
		t.Error("the heading is in the prompt with nothing under it")
	}

	block := personalContextBlock([]db.PersonalEntry{
		{Subject: "identity", Content: "The user is Zemna Braxen."},
		{Subject: "preferences-communication", Content: "The user wants short answers."},
	})
	got := systemInstructionText(time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC), "linux", "amd64", "sh", block, "some context", 5)

	if !strings.Contains(got, "Personal context, things known for certain about the user and their world:") {
		t.Error("the personal context block is missing its heading")
	}
	if !strings.Contains(got, "The user is Zemna Braxen.") || !strings.Contains(got, "The user wants short answers.") {
		t.Error("an entry didn't reach the prompt")
	}
	if !strings.Contains(got, "personal_context") {
		t.Error("the prompt never tells the model where corrections to personal facts go")
	}
}
