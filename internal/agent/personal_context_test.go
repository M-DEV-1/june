package agent

import (
	"context"
	"strings"
	"testing"
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
