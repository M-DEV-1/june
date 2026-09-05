package agent

import (
	"context"
	"errors"
	"ora/internal/db"
	"strings"
	"testing"
	"time"
)

// TestPersonalContextTool_IsNonBlocking keeps it consistent with every other write tool: a store write must not freeze the voice conversation.
func TestPersonalContextTool_IsNonBlocking(t *testing.T) {
	for _, fd := range toolDefinitions()[0].FunctionDeclarations {
		if fd.Name == "personal_context" && fd.Behavior != "NON_BLOCKING" {
			t.Errorf("personal_context behavior is %q, want NON_BLOCKING", fd.Behavior)
		}
	}
}

func TestPersonalContextTool_SetViewDelete(t *testing.T) {
	ctx := context.Background()
	brain := &toolTestBrain{}
	a := &Agent{brain: brain}

	if got := a.executeTool(ctx, "personal_context", map[string]any{
		"action": "set", "subject": "priya-shah", "content": "Priya Shah is the user's colleague at Acme.",
	}); strings.HasPrefix(got, "error:") {
		t.Fatalf("set failed: %s", got)
	}
	if brain.personal["priya-shah"] != "Priya Shah is the user's colleague at Acme." {
		t.Errorf("set wrote %q", brain.personal["priya-shah"])
	}

	view := a.executeTool(ctx, "personal_context", map[string]any{"action": "view"})
	if !strings.Contains(view, "priya-shah") || !strings.Contains(view, "colleague at Acme") {
		t.Errorf("view didn't show the entry:\n%s", view)
	}

	if got := a.executeTool(ctx, "personal_context", map[string]any{"action": "delete", "subject": "priya-shah"}); strings.HasPrefix(got, "error:") {
		t.Fatalf("delete failed: %s", got)
	}
	if _, still := brain.personal["priya-shah"]; still {
		t.Error("delete left the entry behind")
	}
}

func TestPersonalContextTool_RejectsBlanksAndBadActions(t *testing.T) {
	ctx := context.Background()
	a := &Agent{brain: &toolTestBrain{}}

	cases := []struct {
		name string
		args map[string]any
	}{
		{"no subject", map[string]any{"action": "set", "content": "something"}},
		{"blank subject", map[string]any{"action": "set", "subject": "  ", "content": "something"}},
		{"no content", map[string]any{"action": "set", "subject": "identity"}},
		{"blank content", map[string]any{"action": "set", "subject": "identity", "content": " "}},
		{"delete with no subject", map[string]any{"action": "delete"}},
		{"unknown action", map[string]any{"action": "forget-everything"}},
	}
	for _, c := range cases {
		got := a.executeTool(ctx, "personal_context", c.args)
		if !strings.HasPrefix(got, "error:") {
			t.Errorf("%s: want an error, got %q", c.name, got)
		}
		if strings.Contains(got, "%!") || strings.Contains(strings.ToLower(got), "sql") {
			t.Errorf("%s: error text is not plain words: %q", c.name, got)
		}
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

// TestSystemInstruction_PersonalContextBlock checks the entries reach the prompt verbatim, under their own heading, and that the prompt points corrections at the personal_context tool.
func TestSystemInstruction_PersonalContextBlock(t *testing.T) {
	block := personalContextBlock([]db.PersonalEntry{
		{Subject: "identity", Content: "The user is Alex Rivera."},
		{Subject: "preferences-communication", Content: "The user wants short answers."},
	})
	got := systemInstructionText(time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC), "linux", "amd64", "sh", block, "some context", 5)

	if !strings.Contains(got, "Personal context — things known for certain about the user and their world:") {
		t.Error("the personal context block is missing its heading")
	}
	if !strings.Contains(got, "The user is Alex Rivera.") || !strings.Contains(got, "The user wants short answers.") {
		t.Error("an entry didn't reach the prompt")
	}
	if !strings.Contains(got, "personal_context") {
		t.Error("the prompt never tells the model where corrections to personal facts go")
	}
}

// TestSystemInstruction_NoPersonalContextBlockWhenEmpty keeps the prompt from carrying an empty heading on a fresh install.
func TestSystemInstruction_NoPersonalContextBlockWhenEmpty(t *testing.T) {
	if block := personalContextBlock(nil); block != "" {
		t.Errorf("an empty store produced a block: %q", block)
	}
	got := systemInstructionText(time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC), "linux", "amd64", "sh", "", "some context", 5)
	if strings.Contains(got, "Personal context — things known for certain") {
		t.Error("the heading is in the prompt with nothing under it")
	}
}
