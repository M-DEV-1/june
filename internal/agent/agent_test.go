package agent_test

import (
	"context"
	"ora/internal/agent"
	"ora/internal/db"
	"ora/internal/memory"
	"testing"
	"time"
)

// mock hardware & brain

type mockMic struct{}

func (m *mockMic) StartCapture(ctx context.Context) (<-chan []byte, error) { return nil, nil }
func (m *mockMic) Close() error                                            { return nil }
func (m *mockMic) CurrentAmplitude() float64                               { return 0 }

type mockSpeaker struct{}

func (s *mockSpeaker) Play(pcm []byte) error     { return nil }
func (s *mockSpeaker) Flush()                    {}
func (s *mockSpeaker) Close() error              { return nil }
func (s *mockSpeaker) CurrentAmplitude() float64 { return 0 }

type mockBrain struct{}

func (b *mockBrain) GetImplicitContext(ctx context.Context) ([]string, error) { return nil, nil }
func (b *mockBrain) SearchMemory(ctx context.Context, query string) ([]db.MemoryHit, error) {
	return nil, nil
}
func (b *mockBrain) RetrieveRelevant(ctx context.Context, focus string, maxItems int) ([]string, error) {
	return nil, nil
}
func (b *mockBrain) LogNote(ctx context.Context, content, kind string) (int64, error) { return 0, nil }
func (b *mockBrain) GetNotes(ctx context.Context) ([]db.Note, error)                  { return nil, nil }
func (b *mockBrain) NotesOfKindSince(ctx context.Context, kind string, since time.Time) ([]db.Note, error) {
	return nil, nil
}
func (b *mockBrain) UpdateNote(ctx context.Context, id int64, content string) error { return nil }
func (b *mockBrain) DeleteNote(ctx context.Context, id int64) error                 { return nil }
func (b *mockBrain) PersonalContext(ctx context.Context) ([]db.PersonalEntry, error) {
	return nil, nil
}
func (b *mockBrain) SetPersonalContext(ctx context.Context, subject, content string) error {
	return nil
}
func (b *mockBrain) DeletePersonalContext(ctx context.Context, subject string) error { return nil }
func (b *mockBrain) EpisodesForThread(ctx context.Context, threadID int64, limit int) ([]db.Episode, error) {
	return nil, nil
}

func (b *mockBrain) OpenActionItems(ctx context.Context) ([]memory.ActionItem, error) {
	return nil, nil
}
func (b *mockBrain) ListEpisodes(ctx context.Context, q db.EpisodeQuery) ([]db.Episode, error) {
	return nil, nil
}
func (b *mockBrain) SummaryTimeline(ctx context.Context, since, until time.Time) ([]db.WindowSummary, error) {
	return nil, nil
}
func (b *mockBrain) RecallSubject(ctx context.Context, subject string, limit int) ([]string, error) {
	return nil, nil
}
func (b *mockBrain) HybridSearchWindow(ctx context.Context, query, domainFilter string, since, until time.Time, limit int) ([]db.MemoryHit, error) {
	return nil, nil
}
func (b *mockBrain) SaveFold(ctx context.Context, task, result string) (int64, error)   { return 0, nil }
func (b *mockBrain) UnconsumedFolds(ctx context.Context) ([]db.Fold, error)             { return nil, nil }
func (b *mockBrain) ConsumeFold(ctx context.Context, id int64) error                    { return nil }
func (b *mockBrain) SetActionStatus(ctx context.Context, id int64, status string) error { return nil }
func (b *mockBrain) SetActionPriority(ctx context.Context, id int64, priority string) error {
	return nil
}
func (b *mockBrain) SetActionText(ctx context.Context, id int64, text string) error      { return nil }
func (b *mockBrain) UpdateThreadState(ctx context.Context, id int64, state string) error { return nil }
func (b *mockBrain) QueryStore(ctx context.Context, query string, rowCap int) (string, error) {
	return "", nil
}

// behavior

func TestAgent_ConnectFailsWithBadKey(t *testing.T) {
	// inject mocks and fake API key
	mic := &mockMic{}
	a := agent.NewAgent(mic, &mockSpeaker{}, &mockBrain{}, nil, "FAKE_API_KEY")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	micChan, _ := mic.StartCapture(ctx)
	err := a.Connect(ctx, micChan)

	if err == nil {
		t.Fatal("Expected Connect to fail with a fake API key, but it succeeded..")
	}

	t.Logf("Successfully caught API connection error: %+v", err)
}
