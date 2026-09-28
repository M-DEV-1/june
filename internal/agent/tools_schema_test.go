package agent

import (
	"encoding/json"
	"testing"
)

// TestCodexTools_ParameterlessToolKeepsEmptyProperties pins the wire shape of a tool declared with no parameters: read_clipboard must go to the Responses API as {"type":"object","properties":{}}, the shape OpenAI documents for a function that takes nothing, not a bare {"type":"object"}.
func TestCodexTools_ParameterlessToolKeepsEmptyProperties(t *testing.T) {
	var params map[string]any
	for _, tl := range codexTools(toolDefinitions()[0].FunctionDeclarations) {
		if tl.Name == "read_clipboard" {
			params = tl.Parameters
		}
	}
	if params == nil {
		t.Fatal("read_clipboard is not among the Codex tools")
	}
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"properties":{},"type":"object"}` {
		t.Errorf("read_clipboard parameters = %s, want an object with empty properties", raw)
	}
}
