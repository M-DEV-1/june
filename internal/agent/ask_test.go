package agent

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"
)

func TestHandshakePrompt_IncludesImplicitContext(t *testing.T) {
	brain := &toolTestBrain{implicitContext: []string{"[now] Climate Risk Statement Builder ASRS"}}
	a := NewAgent(nil, nil, brain, nil, "")
	instruction, lines := a.HandshakePrompt(t.Context(), time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC))
	if !containsLine(lines, "Climate Risk Statement Builder ASRS") {
		t.Fatalf("expected handshake context lines to carry working state, got %q", lines)
	}
	if !strings.Contains(instruction, "Climate Risk Statement Builder ASRS") {
		t.Fatalf("expected frozen system instruction to embed handshake context, got %q", instruction)
	}
	if !strings.Contains(instruction, "Wednesday, 19 August 2026") {
		t.Errorf("expected date anchor in instruction, got %q", instruction)
	}
}

func TestEvalExecute_BlocksNonMemoryTools(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	got := a.evalExecute(t.Context(), "shell_exec", map[string]any{"command": "rm -rf /"})
	if !strings.Contains(got, "disabled in evals") {
		t.Fatalf("expected shell_exec to be blocked in evals, got %q", got)
	}
	got = a.evalExecute(t.Context(), "save_note", map[string]any{"content": "secret"})
	if !strings.Contains(got, "disabled in evals") {
		t.Fatalf("expected save_note to be blocked in evals, got %q", got)
	}
}

func TestCollectParts_SplitsThoughtFromAnswer(t *testing.T) {
	tr := TurnTrace{}
	collectParts([]*genai.Part{
		{Text: "I should recall Pune", Thought: true},
		{Text: "Hadapsar and Mundhwa.", Thought: false},
	}, &tr)
	if len(tr.Thoughts) != 1 || tr.Thoughts[0] != "I should recall Pune" {
		t.Fatalf("thoughts = %q", tr.Thoughts)
	}
	if tr.Answer != "Hadapsar and Mundhwa." {
		t.Fatalf("answer = %q", tr.Answer)
	}
}

func containsLine(lines []string, needle string) bool {
	for _, l := range lines {
		if strings.Contains(l, needle) {
			return true
		}
	}
	return false
}
