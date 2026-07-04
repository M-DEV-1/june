package agent

import (
	"context"
	"ora/internal/db"
	"strings"
	"testing"
	"time"
)

// toolTestBrain is a minimal ContextReader mock used to exercise
// executeTool's query_memory case. It lives in an internal (package agent,
// not agent_test) test file because executeTool is unexported.
type toolTestBrain struct {
	episodeHits    []db.MemoryHit
	windowEpisodes []db.Episode
	subjectRecall  []string
}

func (b *toolTestBrain) GetImplicitContext(ctx context.Context) ([]string, error) { return nil, nil }
func (b *toolTestBrain) SearchMemory(ctx context.Context, query string) ([]db.MemoryHit, error) {
	return nil, nil
}
func (b *toolTestBrain) RankedEpisodes(ctx context.Context, focus string, limit int) ([]db.MemoryHit, error) {
	return b.episodeHits, nil
}
func (b *toolTestBrain) RetrieveRelevant(ctx context.Context, focus string, maxItems int) ([]string, error) {
	return nil, nil
}
func (b *toolTestBrain) LogNote(ctx context.Context, content, kind string) (int64, error) {
	return 0, nil
}
func (b *toolTestBrain) GetNotes(ctx context.Context) ([]db.Note, error) { return nil, nil }
func (b *toolTestBrain) DeleteNote(ctx context.Context, id int64) error  { return nil }
func (b *toolTestBrain) EpisodesInWindow(ctx context.Context, since, until time.Time, limit int) ([]db.Episode, error) {
	return b.windowEpisodes, nil
}
func (b *toolTestBrain) RecallSubject(ctx context.Context, subject string, limit int) ([]string, error) {
	return b.subjectRecall, nil
}

// TestExecuteTool_QueryMemory_SurfacesEpisodeHit verifies that the
// query_memory tool case merges ranked episode hits (raw screen-capture
// history) alongside SearchMemory hits, labeled "[episode] ...", so the
// model can search episode specifics that SearchMemory alone doesn't cover.
func TestExecuteTool_QueryMemory_SurfacesEpisodeHit(t *testing.T) {
	brain := &toolTestBrain{
		episodeHits: []db.MemoryHit{
			{Source: "episode", Content: "saw the Riddler press conference on Gotham News", RefID: 1},
		},
	}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool("query_memory", map[string]any{"query": "Riddler"})

	if !strings.Contains(result, "[episode] saw the Riddler press conference") {
		t.Errorf("expected query_memory result to surface episode hit, got: %q", result)
	}
}

// TestExecuteTool_Recall_WindowPath verifies that calling the "recall" tool
// with a "window" arg (and no "subject") surfaces the brain's canned timeline
// (EpisodesInWindow), formatted chronologically as "[HH:MM] app — title: ...".
func TestExecuteTool_Recall_WindowPath(t *testing.T) {
	morning := time.Date(2026, 7, 4, 8, 30, 0, 0, time.UTC)
	noon := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	brain := &toolTestBrain{
		windowEpisodes: []db.Episode{
			{ID: 1, CreatedAt: morning, App: "Mail", Title: "Inbox", ScreenText: "reading morning emails"},
			{ID: 2, CreatedAt: noon, App: "VSCode", Title: "main.go", ScreenText: "writing the consolidation layer"},
		},
	}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool("recall", map[string]any{"window": "today"})

	if !strings.Contains(result, "Mail") || !strings.Contains(result, "Inbox") {
		t.Errorf("expected recall (window) to surface the morning episode, got: %q", result)
	}
	if !strings.Contains(result, "VSCode") || !strings.Contains(result, "main.go") {
		t.Errorf("expected recall (window) to surface the noon episode, got: %q", result)
	}
	if !strings.Contains(result, "08:30") || !strings.Contains(result, "12:00") {
		t.Errorf("expected recall (window) to include HH:MM timestamps, got: %q", result)
	}
}

// TestExecuteTool_Recall_SubjectPath verifies that calling the "recall" tool
// with a "subject" arg surfaces the brain's canned RecallSubject lines
// (thread + episode fusion) rather than the window timeline.
func TestExecuteTool_Recall_SubjectPath(t *testing.T) {
	brain := &toolTestBrain{
		subjectRecall: []string{
			"[thread] DeepSeek — studying post-training",
			"[episode] reading the DeepSeek post-training paper introduction",
		},
	}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool("recall", map[string]any{"subject": "DeepSeek"})

	if !strings.Contains(result, "[thread] DeepSeek — studying post-training") {
		t.Errorf("expected recall (subject) to surface thread line, got: %q", result)
	}
	if !strings.Contains(result, "[episode] reading the DeepSeek post-training paper introduction") {
		t.Errorf("expected recall (subject) to surface episode line, got: %q", result)
	}
}
