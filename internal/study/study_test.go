package study

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureReplay is a trimmed stand-in for one of evals/track5_replay.go's side-by-side files.
const fixtureReplay = `# Counterfactual replay — session 2026-08-30 14:17:22

**USER (voice):** could you possibly gather more context

**LIVE (gemini):** (no spoken reply logged)

**CLAUDE** (tools: none)**:** On it, give me a second.

Verdict: **claude** — Reply B responds, reply A is empty.
`

// fixtureTrace is one night's trace JSONL: two lines with fields readTrace should keep, one line that has none, and one line that is not valid JSON at all.
const fixtureTrace = `{"kind":"verdicts","reply":"TRACE_REPLY_TEXT","at":"2026-08-29T01:00:00Z"}
not json at all
{"other":"ignored, no matching field"}
{"stage":"extract","raw":"TRACE_RAW_TEXT"}
`

func writeFixtures(t *testing.T) (replayPath, tracePath string) {
	t.Helper()
	dir := t.TempDir()
	replayPath = filepath.Join(dir, "2026-08-30T14-17-22.md")
	tracePath = filepath.Join(dir, "2026-08-29.jsonl")
	if err := os.WriteFile(replayPath, []byte(fixtureReplay), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tracePath, []byte(fixtureTrace), 0644); err != nil {
		t.Fatal(err)
	}
	return replayPath, tracePath
}

// TestStudy_GathersBothSourcesIntoOnePromptAndParsesProseWrappedJSON covers the end-to-end pass: the prompt handed to the teacher carries both the replay and the trace material, a reply that wraps its JSON in prose and a fence still parses, the report and lessons files land on disk, and the corrupt trace line is reported as skipped rather than failing the run.
func TestStudy_GathersBothSourcesIntoOnePromptAndParsesProseWrappedJSON(t *testing.T) {
	replayPath, tracePath := writeFixtures(t)
	outDir := t.TempDir()

	var capturedPrompt string
	teach := func(ctx context.Context, prompt string) (string, error) {
		capturedPrompt = prompt
		return "Sure, here you go:\n```json\n{\"lessons\":[{\"title\":\"Say when unsure\",\"lesson\":\"When the user's words look garbled, say so instead of guessing.\",\"evidence\":\"trace 2026-08-29\"}],\"summary\":\"One lesson found.\"}\n```", nil
	}

	res, err := Study(context.Background(), teach, []string{replayPath}, []string{tracePath}, outDir)
	if err != nil {
		t.Fatal(err)
	}
	if res.ReplaysRead != 1 || res.TracesRead != 1 {
		t.Errorf("read counts: replays=%d traces=%d", res.ReplaysRead, res.TracesRead)
	}
	if res.LinesSkipped != 1 {
		t.Errorf("lines skipped: want 1, got %d", res.LinesSkipped)
	}
	if res.LessonsAdded != 1 {
		t.Errorf("lessons added: want 1, got %d", res.LessonsAdded)
	}

	for _, want := range []string{"could you possibly gather more context", "TRACE_REPLY_TEXT", "MATERIAL INVENTORY"} {
		if !strings.Contains(capturedPrompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}

	report, err := os.ReadFile(res.ReportPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"One lesson found.", "### Say when unsure", "garbled"} {
		if !strings.Contains(string(report), want) {
			t.Errorf("report missing %q", want)
		}
	}

	lessons, err := os.ReadFile(res.LessonsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(lessons), "## Say when unsure") {
		t.Errorf("lessons.md missing the new lesson: %s", lessons)
	}
}

// TestStudy_LessonsDedupAcrossTwoRuns checks the cumulative file: a second run whose teacher repeats a title from the first run must not duplicate it, a title repeated within one reply lands once, and a genuinely new title still gets appended.
func TestStudy_LessonsDedupAcrossTwoRuns(t *testing.T) {
	replayPath, tracePath := writeFixtures(t)
	outDir := t.TempDir()

	teach1 := func(ctx context.Context, prompt string) (string, error) {
		return `{"lessons":[{"title":"Same title","lesson":"First run's wording.","evidence":"e1"}],"summary":"s1"}`, nil
	}
	res1, err := Study(context.Background(), teach1, []string{replayPath}, []string{tracePath}, outDir)
	if err != nil {
		t.Fatal(err)
	}
	if res1.LessonsAdded != 1 {
		t.Fatalf("first run: want 1 lesson added, got %d", res1.LessonsAdded)
	}

	teach2 := func(ctx context.Context, prompt string) (string, error) {
		return `{"lessons":[{"title":"Same title","lesson":"Second run repeats the title.","evidence":"e2"},{"title":"A new title","lesson":"Genuinely new lesson.","evidence":"e3"},{"title":"A new title","lesson":"The same lesson twice in one reply.","evidence":"e4"}],"summary":"s2"}`, nil
	}
	res2, err := Study(context.Background(), teach2, []string{replayPath}, []string{tracePath}, outDir)
	if err != nil {
		t.Fatal(err)
	}
	if res2.LessonsAdded != 1 {
		t.Errorf("second run: want 1 new lesson added (the duplicate title skipped), got %d", res2.LessonsAdded)
	}

	lessons, err := os.ReadFile(res2.LessonsPath)
	if err != nil {
		t.Fatal(err)
	}
	body := string(lessons)
	if strings.Count(body, "## Same title") != 1 {
		t.Errorf("duplicate title should appear once, found %d times: %s", strings.Count(body, "## Same title"), body)
	}
	if strings.Count(body, "## A new title") != 1 {
		t.Errorf("a title repeated within one reply should land once, found %d times: %s", strings.Count(body, "## A new title"), body)
	}
	if strings.Contains(body, "Second run repeats the title.") {
		t.Error("the duplicate's second-run wording should not have been appended")
	}
}
