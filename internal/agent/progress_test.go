package agent

import (
	"context"
	"strings"
	"testing"
)

// WithToolObserver/ObserveTool round-trip: a context carrying an observer must deliver both name and summary to it exactly as given.
func TestWithToolObserver_RoundTrips(t *testing.T) {
	var got []string
	ctx := WithToolObserver(context.Background(), func(name, summary string) {
		got = append(got, name+":"+summary)
	})
	ObserveTool(ctx, "click", "pressing Reload")
	if len(got) != 1 || got[0] != "click:pressing Reload" {
		t.Errorf("observer round-trip = %v", got)
	}
}

// A context with no observer attached, or one attached as an explicit nil, must be a silent no-op rather than a nil-func panic — askText and askVoice call this on every tool hop of every turn, most of which (a plain /ask with no live window watching) carry no observer at all.
func TestObserveTool_NoObserverIsANoop(t *testing.T) {
	ObserveTool(context.Background(), "click", "pressing Reload")
	ObserveTool(WithToolObserver(context.Background(), nil), "click", "pressing Reload")
}

// The gate that lets a text ask run tools without AllowEvalWrites has to admit whatever a real /ask turn needs, since the daemon never sets AllowEvalWrites: it now also allows the store-writing tools that need no HITL approval (save_note, personal_context, revise, action_items, query_store, open_url), while the ones gated behind ToolApprovalChan (shell_exec, read_file, list_files) stay blocked, because nothing in the daemon reads that channel to answer the prompt.
func TestEvalExecute_AllowsUnapprovedWriteTools(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	for _, tool := range []string{"save_note", "personal_context", "revise", "action_items", "query_store", "open_url"} {
		if got := a.evalExecute(context.Background(), tool, map[string]any{}); strings.Contains(got, "not available in an ask") {
			t.Errorf("%s must be allowed without AllowEvalWrites, got %q", tool, got)
		}
	}
	for _, tool := range []string{"shell_exec", "read_file", "list_files"} {
		if got := a.evalExecute(context.Background(), tool, map[string]any{}); !strings.Contains(got, "not available in an ask") {
			t.Errorf("%s must stay behind the gate, got %q", tool, got)
		}
	}
}
