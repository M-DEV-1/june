package agent

import (
	"context"
	"ora/internal/db"
	"strings"
	"testing"
)

// toolTestBrain is a minimal ContextReader mock used to exercise
// executeTool's query_memory case. It lives in an internal (package agent,
// not agent_test) test file because executeTool is unexported.
type toolTestBrain struct {
	episodeHits []db.MemoryHit
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
