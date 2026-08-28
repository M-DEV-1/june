package agent

import (
	"context"
	"fmt"
	"ora/internal/db"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"
)

// toolTestBrain is a minimal ContextReader mock used to exercise executeTool's query_memory case. It lives in an internal (package agent, not agent_test) test file because executeTool is unexported.
type toolTestBrain struct {
	episodeHits    []db.MemoryHit
	windowEpisodes []db.Episode
	subjectRecall  []string

	// searchMemoryResult/searchMemoryCalledFocus back SearchMemory — used by buildHandshakeContext's tests (F2) to verify the [working]-buffer focus signal actually drives a SearchMemory call.
	searchMemoryResult      []db.MemoryHit
	searchMemoryCalledFocus string

	// hybridHits/capturedDomain back HybridSearch: configurable return value plus a capture of the domainFilter arg, same field-per-method style as the rest of this mock.
	hybridHits     []db.MemoryHit
	capturedDomain string
	// capturedHybridLimit records the limit HybridSearch was asked for, so a test can assert query_memory over-fetches when it has a since/until window to post-filter with.
	capturedHybridLimit int

	// capturedEpisodeQuery records the last db.EpisodeQuery passed to ListEpisodes, so tests can assert on fields (e.g. NewestFirst) the tool is supposed to set without windowEpisodes itself needing to simulate real sort order.
	capturedEpisodeQuery db.EpisodeQuery

	// implicitContext backs GetImplicitContext — used by HandshakePrompt tests so the frozen handshake string can be asserted without a real sqlite store.
	implicitContext []string

	// retrieveRelevantResult/capturedRetrieveFocus back RetrieveRelevant. retrieveRelevantCalled, if non-nil, receives the focus arg right when RetrieveRelevant is invoked — lets a test synchronize on "the async voice-recall retrieval actually happened" (from receiveLoop's goroutine) without racing on capturedRetrieveFocus directly, same pattern as foldSaved below.
	retrieveRelevantResult []string
	capturedRetrieveFocus  string
	retrieveRelevantCalled chan string
	// retrieveRelevantBlocksOnCtx makes RetrieveRelevant hang until its ctx is done instead of returning immediately — simulates a slow/hung embed call for textSendLoop's per-call timeout test (W3).
	retrieveRelevantBlocksOnCtx bool

	// loggedNoteContent/loggedNoteKind capture LogNote's args for the save_note tool's tests; logNoteErr lets a test force LogNote to fail.
	loggedNoteContent string
	loggedNoteKind    string
	logNoteErr        error

	// updatedNoteID/updatedNoteContent capture UpdateNote's args for the update_note tool's tests; updateNoteErr forces it to fail.
	updatedNoteID      int64
	updatedNoteContent string
	updateNoteErr      error

	// updatedThreadID/updatedThreadState capture UpdateThreadState's args for the fix_thread tool's tests; updateThreadErr forces it to fail.
	updatedThreadID    int64
	updatedThreadState string
	updateThreadErr    error

	// deletedNoteID captures DeleteNote's arg for the delete_note tool's tests; deleteNoteErr forces it to fail.
	deletedNoteID int64
	deleteNoteErr error

	// savedFoldTask/savedFoldResult capture SaveFold's args; unconsumedFolds backs UnconsumedFolds' return value; consumedFoldIDs records every id passed to ConsumeFold, in order — for the branch dead-session-fallback and next-session-surfacing tests.
	savedFoldTask   string
	savedFoldResult string
	saveFoldErr     error
	unconsumedFolds []db.Fold
	consumedFoldIDs []int64
	// foldSaved, if non-nil, receives a value right after SaveFold records its args — lets a test synchronize on "the fallback persistence actually happened" (from a background goroutine) without racing on the plain fields above. Buffered 1 so SaveFold's send never blocks.
	foldSaved chan struct{}
}

func (b *toolTestBrain) GetImplicitContext(ctx context.Context) ([]string, error) {
	return b.implicitContext, nil
}
func (b *toolTestBrain) SearchMemory(ctx context.Context, query string) ([]db.MemoryHit, error) {
	b.searchMemoryCalledFocus = query
	return b.searchMemoryResult, nil
}
func (b *toolTestBrain) RankedEpisodes(ctx context.Context, focus string, limit int) ([]db.MemoryHit, error) {
	return b.episodeHits, nil
}
func (b *toolTestBrain) RetrieveRelevant(ctx context.Context, focus string, maxItems int) ([]string, error) {
	b.capturedRetrieveFocus = focus
	if b.retrieveRelevantCalled != nil {
		b.retrieveRelevantCalled <- focus
	}
	if b.retrieveRelevantBlocksOnCtx {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return b.retrieveRelevantResult, nil
}
func (b *toolTestBrain) LogNote(ctx context.Context, content, kind string) (int64, error) {
	b.loggedNoteContent = content
	b.loggedNoteKind = kind
	if b.logNoteErr != nil {
		return 0, b.logNoteErr
	}
	return 1, nil
}
func (b *toolTestBrain) GetNotes(ctx context.Context) ([]db.Note, error) { return nil, nil }
func (b *toolTestBrain) UpdateNote(ctx context.Context, id int64, content string) error {
	b.updatedNoteID = id
	b.updatedNoteContent = content
	return b.updateNoteErr
}
func (b *toolTestBrain) UpdateThreadState(ctx context.Context, id int64, state string) error {
	b.updatedThreadID = id
	b.updatedThreadState = state
	return b.updateThreadErr
}
func (b *toolTestBrain) DeleteNote(ctx context.Context, id int64) error {
	b.deletedNoteID = id
	return b.deleteNoteErr
}
func (b *toolTestBrain) EpisodesInWindow(ctx context.Context, since, until time.Time, limit int) ([]db.Episode, error) {
	return b.windowEpisodes, nil
}
func (b *toolTestBrain) ListEpisodes(ctx context.Context, q db.EpisodeQuery) ([]db.Episode, error) {
	b.capturedEpisodeQuery = q
	var out []db.Episode
	for _, e := range b.windowEpisodes {
		if q.App != "" && !strings.Contains(strings.ToLower(e.App), strings.ToLower(q.App)) {
			continue
		}
		out = append(out, e)
	}
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}
func (b *toolTestBrain) RecallSubject(ctx context.Context, subject string, limit int) ([]string, error) {
	return b.subjectRecall, nil
}

// HybridSearch returns the configured hybridHits and records the domainFilter it was called with, so tests can assert executeTool's "query_memory" case wires args["domain"] through correctly.
func (b *toolTestBrain) HybridSearch(ctx context.Context, query, domainFilter string, limit int) ([]db.MemoryHit, error) {
	b.capturedDomain = domainFilter
	b.capturedHybridLimit = limit
	return b.hybridHits, nil
}

func (b *toolTestBrain) SaveFold(ctx context.Context, task, result string) (int64, error) {
	b.savedFoldTask = task
	b.savedFoldResult = result
	if b.foldSaved != nil {
		select {
		case b.foldSaved <- struct{}{}:
		default:
		}
	}
	if b.saveFoldErr != nil {
		return 0, b.saveFoldErr
	}
	return 1, nil
}

func (b *toolTestBrain) UnconsumedFolds(ctx context.Context) ([]db.Fold, error) {
	return b.unconsumedFolds, nil
}

func (b *toolTestBrain) ConsumeFold(ctx context.Context, id int64) error {
	b.consumedFoldIDs = append(b.consumedFoldIDs, id)
	return nil
}

// recallBounds turns the model's since/until args into a concrete window: explicit RFC3339 instants are honored verbatim, an omitted until means "up to now", an omitted since means midnight of now's day, a bare calendar date spans that whole day, and anything unparseable or backwards is an error rather than a silently wrong window.
func TestRecallBounds(t *testing.T) {
	now := time.Date(2026, 7, 6, 12, 44, 0, 0, time.UTC)
	day := func(y int, m time.Month, d, h, min, sec int) time.Time {
		return time.Date(y, m, d, h, min, sec, 0, now.Location())
	}

	cases := []struct {
		name                 string
		since, until         string
		wantSince, wantUntil time.Time
		wantErr              bool
	}{
		{name: "explicit range", since: "2026-07-05T00:00:00Z", until: "2026-07-05T23:59:59Z", wantSince: day(2026, 7, 5, 0, 0, 0), wantUntil: day(2026, 7, 5, 23, 59, 59)},
		{name: "empty until means now", since: "2026-07-06T08:00:00Z", wantSince: day(2026, 7, 6, 8, 0, 0), wantUntil: now},
		{name: "empty since means start of today", wantSince: day(2026, 7, 6, 0, 0, 0), wantUntil: now},
		{name: "bare date spans the whole day", since: "2026-07-05", until: "2026-07-05", wantSince: day(2026, 7, 5, 0, 0, 0), wantUntil: day(2026, 7, 5, 23, 59, 59)},
		{name: "unparseable since", since: "last tuesday", wantErr: true},
		{name: "since after until", since: "2026-07-10T00:00:00Z", until: "2026-07-05T00:00:00Z", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			since, until, err := recallBounds(tc.since, tc.until, now)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("recallBounds(%q, %q) = %v..%v, want an error", tc.since, tc.until, since, until)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !since.Equal(tc.wantSince) {
				t.Errorf("since = %v, want %v", since, tc.wantSince)
			}
			if !until.Equal(tc.wantUntil) {
				t.Errorf("until = %v, want %v", until, tc.wantUntil)
			}
		})
	}
}

// TestToolDefinitions_IncludesUpdateAndDeleteNote verifies update_note and delete_note are declared to the model (not just wired in executeTool) — with an id parameter, and content required on update_note but not delete_note.
func TestToolDefinitions_IncludesUpdateAndDeleteNote(t *testing.T) {
	var updateNote, deleteNote *genai.FunctionDeclaration
	for _, tool := range toolDefinitions() {
		for _, fd := range tool.FunctionDeclarations {
			switch fd.Name {
			case "update_note":
				updateNote = fd
			case "delete_note":
				deleteNote = fd
			}
		}
	}

	if updateNote == nil {
		t.Fatal("update_note tool declaration not found")
	}
	if _, ok := updateNote.Parameters.Properties["id"]; !ok {
		t.Error("expected update_note to declare an \"id\" parameter")
	}
	if _, ok := updateNote.Parameters.Properties["content"]; !ok {
		t.Error("expected update_note to declare a \"content\" parameter")
	}
	if !slices.Contains(updateNote.Parameters.Required, "id") || !slices.Contains(updateNote.Parameters.Required, "content") {
		t.Errorf("expected update_note to require both id and content, got required=%v", updateNote.Parameters.Required)
	}

	if deleteNote == nil {
		t.Fatal("delete_note tool declaration not found")
	}
	if _, ok := deleteNote.Parameters.Properties["id"]; !ok {
		t.Error("expected delete_note to declare an \"id\" parameter")
	}
	if !slices.Contains(deleteNote.Parameters.Required, "id") {
		t.Errorf("expected delete_note to require id, got required=%v", deleteNote.Parameters.Required)
	}
}

// TestExecuteTool_QueryMemory_SurfacesEpisodeHit verifies query_memory surfaces episode hits (raw screen-capture history), labeled "[episode] ...", so the model can search episode specifics.
func TestExecuteTool_QueryMemory_SurfacesEpisodeHit(t *testing.T) {
	brain := &toolTestBrain{
		hybridHits: []db.MemoryHit{
			{Source: "episode", Content: "saw the Riddler press conference on Gotham News", RefID: 1},
		},
	}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "query_memory", map[string]any{"query": "Riddler"})

	if !strings.Contains(result, "[episode] saw the Riddler press conference") {
		t.Errorf("expected query_memory result to surface episode hit, got: %q", result)
	}
}

func TestExecuteTool_QueryMemory_FiltersByApp(t *testing.T) {
	brain := &toolTestBrain{
		hybridHits: []db.MemoryHit{
			{Source: "episode", App: "Slack", Title: "ora", Content: "retrieval thread"},
			{Source: "episode", App: "Firefox", Title: "Suits", Content: "watching"},
			{Source: "note", Content: "user likes go"},
		},
	}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")
	result := a.executeTool(context.Background(), "query_memory", map[string]any{"query": "anything", "app": "slack"})
	if !strings.Contains(result, "Slack") || strings.Contains(result, "Firefox") || strings.Contains(result, "user likes go") {
		t.Fatalf("app filter leaked: %q", result)
	}
}

func TestExecuteTool_GetRecent(t *testing.T) {
	now := time.Date(2026, 8, 18, 15, 4, 0, 0, time.UTC)
	brain := &toolTestBrain{
		windowEpisodes: []db.Episode{
			{App: "Slack", Title: "ora", ScreenText: "reviewing PR", UserActivity: "reviewing a pull request", CreatedAt: now, ImagePath: "frames/2.jpg"},
			{App: "Firefox", Title: "Suits", ScreenText: "watching", CreatedAt: now.Add(-time.Hour)},
		},
	}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")
	result := a.executeTool(context.Background(), "get_recent", map[string]any{"limit": float64(5)})
	if !strings.Contains(result, "Slack") || !strings.Contains(result, "[img]") {
		t.Fatalf("get_recent: %q", result)
	}
	onlySlack := a.executeTool(context.Background(), "get_recent", map[string]any{"app": "firefox"})
	if !strings.Contains(onlySlack, "Firefox") || strings.Contains(onlySlack, "Slack") {
		t.Fatalf("get_recent app: %q", onlySlack)
	}
}

func TestExecuteTool_Recall_TimelineHonorsApp(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	brain := &toolTestBrain{
		windowEpisodes: []db.Episode{
			{App: "Slack", Title: "ora", ScreenText: "thread", CreatedAt: now},
			{App: "Code", Title: "main.go", ScreenText: "editing", CreatedAt: now},
		},
	}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")
	result := a.executeTool(context.Background(), "recall", map[string]any{
		"since": "2026-08-18", "until": "2026-08-18", "app": "code",
	})
	if !strings.Contains(result, "Code") || strings.Contains(result, "Slack") {
		t.Fatalf("recall app: %q", result)
	}
}

// TestExecuteTool_Recall_TimelinePath verifies that calling the "recall" tool with since/until args (and no "subject") surfaces the brain's canned timeline (EpisodesInWindow), formatted chronologically as "[HH:MM] app — title: ...".
// This is the flagship "walk me through July 4th" path: the model resolves the human phrase into ISO bounds and the tool honors them.
func TestExecuteTool_Recall_TimelinePath(t *testing.T) {
	// Fixtures use time.Local explicitly (not time.UTC): recall now renders timestamps
	// converted to the user's local zone, so a Local fixture round-trips through that
	// conversion as a no-op and the asserted HH:MM below stays correct regardless of
	// which zone the test machine runs in.
	morning := time.Date(2026, 7, 4, 8, 30, 0, 0, time.Local)
	noon := time.Date(2026, 7, 4, 12, 0, 0, 0, time.Local)
	brain := &toolTestBrain{
		windowEpisodes: []db.Episode{
			{ID: 1, CreatedAt: morning, App: "Mail", Title: "Inbox", ScreenText: "reading morning emails"},
			{ID: 2, CreatedAt: noon, App: "VSCode", Title: "main.go", ScreenText: "writing the consolidation layer"},
		},
	}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "recall", map[string]any{
		"since": "2026-07-04T00:00:00Z",
		"until": "2026-07-04T23:59:59Z",
	})

	if !strings.Contains(result, "Mail") || !strings.Contains(result, "Inbox") {
		t.Errorf("expected recall (timeline) to surface the morning episode, got: %q", result)
	}
	if !strings.Contains(result, "VSCode") || !strings.Contains(result, "main.go") {
		t.Errorf("expected recall (timeline) to surface the noon episode, got: %q", result)
	}
	if !strings.Contains(result, "08:30") || !strings.Contains(result, "12:00") {
		t.Errorf("expected recall (timeline) to include HH:MM timestamps, got: %q", result)
	}
}

// TestExecuteTool_Recall_MultiDayShowsDates verifies that a timeline spanning more than one day labels each episode with its date, not just HH:MM — now that since/until can cover a range like "last week", a bare time would be ambiguous across days.
func TestExecuteTool_Recall_MultiDayShowsDates(t *testing.T) {
	// time.Local (not time.UTC) so the local-time rendering conversion is a no-op here too — see TimelinePath's comment above.
	day1 := time.Date(2026, 7, 4, 8, 30, 0, 0, time.Local)
	day2 := time.Date(2026, 7, 5, 9, 15, 0, 0, time.Local)
	brain := &toolTestBrain{
		windowEpisodes: []db.Episode{
			{ID: 1, CreatedAt: day1, App: "Mail", Title: "Inbox", ScreenText: "morning emails"},
			{ID: 2, CreatedAt: day2, App: "Brave", Title: "manga", ScreenText: "reading chapter 29"},
		},
	}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "recall", map[string]any{
		"since": "2026-07-04",
		"until": "2026-07-05",
	})

	if !strings.Contains(result, "Jul 4") || !strings.Contains(result, "Jul 5") {
		t.Errorf("expected multi-day timeline to label each episode's date, got: %q", result)
	}
}

// TestExecuteTool_Recall_UsesNewestFirst verifies the "recall" tool's timeline path asks ListEpisodes for the newest episodes in the window, not the default oldest-first sort — a real day can exceed the 50-episode cap, and oldest-first would return only the start of the window and silently stop there.
func TestExecuteTool_Recall_UsesNewestFirst(t *testing.T) {
	brain := &toolTestBrain{}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	a.executeTool(context.Background(), "recall", map[string]any{
		"since": "2026-07-04", "until": "2026-07-04",
	})

	if !brain.capturedEpisodeQuery.NewestFirst {
		t.Error("expected recall's ListEpisodes call to set NewestFirst: true")
	}
}

// TestExecuteTool_Recall_TimestampsRenderInLocalTime verifies the timeline path renders each episode's timestamp converted to the user's local zone, not left in whatever zone it happens to be stored in (UTC, in practice) — otherwise every time shown to the user is off by the UTC offset and can even show the wrong date.
// The fixture uses a zone offset guaranteed to differ from the test machine's Local zone, so a bug that renders the stored zone verbatim shows up as the wrong wall-clock hour.
func TestExecuteTool_Recall_TimestampsRenderInLocalTime(t *testing.T) {
	_, localOffset := time.Now().Local().Zone()
	fixedZone := time.FixedZone("FIXED", localOffset+3*3600)
	created := time.Date(2026, 7, 4, 8, 30, 0, 0, fixedZone)

	brain := &toolTestBrain{
		windowEpisodes: []db.Episode{
			{ID: 1, CreatedAt: created, App: "Mail", Title: "Inbox", ScreenText: "reading morning emails"},
		},
	}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "recall", map[string]any{
		"since": "2026-07-04", "until": "2026-07-04",
	})

	wantLocal := created.In(time.Local).Format("Jan 2 15:04")
	wantStoredZone := created.Format("Jan 2 15:04")
	if !strings.Contains(result, wantLocal) {
		t.Errorf("expected recall to render the timestamp in local time (%s), got: %q", wantLocal, result)
	}
	if strings.Contains(result, wantStoredZone) {
		t.Errorf("recall rendered the timestamp in its stored zone (%s) instead of local time, got: %q", wantStoredZone, result)
	}
}

// TestExecuteTool_Recall_NonStringSinceErrors verifies that a present-but-wrong-typed "since" arg (e.g. the model sends a JSON number instead of a string) surfaces an explicit error instead of silently coercing to "" and falling through to the default window, which the model could never distinguish from an intentional "default to today" call.
func TestExecuteTool_Recall_NonStringSinceErrors(t *testing.T) {
	brain := &toolTestBrain{
		windowEpisodes: []db.Episode{
			{ID: 1, CreatedAt: time.Now(), App: "Mail", Title: "Inbox", ScreenText: "should not be reached"},
		},
	}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "recall", map[string]any{"since": float64(20260704)})

	if !strings.Contains(result, "error") {
		t.Errorf("expected an explicit error for a non-string since arg, got: %q", result)
	}
	if strings.Contains(result, "should not be reached") {
		t.Errorf("non-string since must not silently fall through to the default window, got: %q", result)
	}
}

// TestExecuteTool_Recall_NonStringUntilErrors is the "until" analogue of TestExecuteTool_Recall_NonStringSinceErrors.
func TestExecuteTool_Recall_NonStringUntilErrors(t *testing.T) {
	brain := &toolTestBrain{}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "recall", map[string]any{"until": 12345})

	if !strings.Contains(result, "error") {
		t.Errorf("expected an explicit error for a non-string until arg, got: %q", result)
	}
}

// TestExecuteTool_Recall_ReversedRangeErrors verifies that since > until is rejected explicitly rather than silently returning "no episodes in that window" — a result indistinguishable from a genuinely empty day.
func TestExecuteTool_Recall_ReversedRangeErrors(t *testing.T) {
	brain := &toolTestBrain{}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "recall", map[string]any{
		"since": "2026-07-10T00:00:00Z",
		"until": "2026-07-05T00:00:00Z",
	})

	if !strings.Contains(result, "error") {
		t.Errorf("expected an explicit error for a reversed since/until range, got: %q", result)
	}
	if strings.Contains(result, "no episodes in that window") {
		t.Errorf("reversed range must not be reported as a genuinely empty window, got: %q", result)
	}
	// the range parsed fine — it's the ordering that's wrong, so the message must not claim a date-format problem that doesn't exist.
	if strings.Contains(result, "real date") {
		t.Errorf("reversed-range error should not lead with a format complaint, got: %q", result)
	}
	if !strings.Contains(result, "runs backwards") {
		t.Errorf("expected the actual ordering problem to be stated, got: %q", result)
	}
}

// TestExecuteTool_Recall_SubjectPath verifies that calling the "recall" tool with a "subject" arg surfaces the brain's canned RecallSubject lines (thread + episode fusion) rather than the window timeline.
func TestExecuteTool_Recall_SubjectPath(t *testing.T) {
	brain := &toolTestBrain{
		subjectRecall: []string{
			"[thread] DeepSeek — studying post-training",
			"[episode] reading the DeepSeek post-training paper introduction",
		},
	}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "recall", map[string]any{"subject": "DeepSeek"})

	if !strings.Contains(result, "[thread] DeepSeek — studying post-training") {
		t.Errorf("expected recall (subject) to surface thread line, got: %q", result)
	}
	if !strings.Contains(result, "[episode] reading the DeepSeek post-training paper introduction") {
		t.Errorf("expected recall (subject) to surface episode line, got: %q", result)
	}
}

// TestExecuteTool_Recall_SubjectWithApp_AnswersAndSaysTheFilterWasIgnored verifies that combining "subject" with "app" answers the subject question and says the app filter was not applied — RecallSubject has no app-filtering parameter to honor, so the one thing the tool must not do is return unfiltered results as if it had.
func TestExecuteTool_Recall_SubjectWithApp_AnswersAndSaysTheFilterWasIgnored(t *testing.T) {
	brain := &toolTestBrain{
		subjectRecall: []string{"[thread] DeepSeek — studying post-training"},
	}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "recall", map[string]any{"subject": "DeepSeek", "app": "slack"})

	if !strings.Contains(result, "[thread] DeepSeek") {
		t.Errorf("expected the subject recall to answer, got %q", result)
	}
	if !strings.Contains(result, "subject recall ignores the app filter") {
		t.Errorf("expected the dropped app filter called out, got %q", result)
	}
}

// TestToolDefinitions_QueryMemory_HasDomainParam asserts query_memory declares an optional "domain" string parameter.
func TestToolDefinitions_QueryMemory_HasDomainParam(t *testing.T) {
	var queryMemory *genai.FunctionDeclaration
	for _, tool := range toolDefinitions() {
		for _, fd := range tool.FunctionDeclarations {
			if fd.Name == "query_memory" {
				queryMemory = fd
			}
		}
	}
	if queryMemory == nil {
		t.Fatalf("query_memory tool declaration not found")
	}
	if queryMemory.Parameters == nil || queryMemory.Parameters.Properties == nil {
		t.Fatalf("query_memory has no parameters/properties defined")
	}
	if _, ok := queryMemory.Parameters.Properties["domain"]; !ok {
		t.Errorf(`expected query_memory to declare an optional "domain" parameter, got properties: %v`, queryMemory.Parameters.Properties)
	}
	// domain must be optional: it must NOT appear in Required.
	for _, req := range queryMemory.Parameters.Required {
		if req == "domain" {
			t.Errorf(`expected "domain" to be optional, but found it in Required: %v`, queryMemory.Parameters.Required)
		}
	}
}

// TestExecuteTool_QueryMemory_NoDomainArg_CallsHybridSearchWithEmptyFilter verifies that omitting the optional "domain" arg calls a.brain.HybridSearch with domainFilter="" (search everything, weighted toward the current domain) rather than erroring on a missing optional arg.
func TestExecuteTool_QueryMemory_NoDomainArg_CallsHybridSearchWithEmptyFilter(t *testing.T) {
	brain := &toolTestBrain{capturedDomain: "sentinel-should-be-overwritten"}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	a.executeTool(context.Background(), "query_memory", map[string]any{"query": "x"})

	if brain.capturedDomain != "" {
		t.Errorf(`expected HybridSearch to be called with domainFilter="" when "domain" is absent, got %q`, brain.capturedDomain)
	}
}

// TestExecuteTool_QueryMemory_DomainArg_CallsHybridSearchWithDomainFilter verifies that a present "domain" arg ("work") is passed through verbatim as HybridSearch's domainFilter.
func TestExecuteTool_QueryMemory_DomainArg_CallsHybridSearchWithDomainFilter(t *testing.T) {
	brain := &toolTestBrain{}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	a.executeTool(context.Background(), "query_memory", map[string]any{"query": "x", "domain": "work"})

	if brain.capturedDomain != "work" {
		t.Errorf(`expected HybridSearch to be called with domainFilter="work", got %q`, brain.capturedDomain)
	}
}

// TestExecuteTool_QueryMemory_FormatsHybridHits verifies the hits HybridSearch returns are formatted into the result string using the same "[%s] %s" line style already used elsewhere for other sources (e.g. "[episode] ...", "[summary] ...").
func TestExecuteTool_QueryMemory_FormatsHybridHits(t *testing.T) {
	brain := &toolTestBrain{
		hybridHits: []db.MemoryHit{
			{Source: "episode", Content: "saw the Riddler press conference on Gotham News"},
			{Source: "summary", Content: "spent the afternoon debugging CUDA OOM errors"},
		},
	}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "query_memory", map[string]any{"query": "Riddler"})

	if !strings.Contains(result, "[episode] saw the Riddler press conference on Gotham News") {
		t.Errorf("expected episode hit formatted as '[episode] ...', got: %q", result)
	}
	if !strings.Contains(result, "[summary] spent the afternoon debugging CUDA OOM errors") {
		t.Errorf("expected summary hit formatted as '[summary] ...', got: %q", result)
	}
}

// TestExecuteTool_QueryMemory_FormatsNoteHitWithRefID verifies note hits carry their ref_id in the surfaced line ("[note#105] ..." instead of just "[note] ..."), unlike every other source — notes are the only source with an update_note/delete_note follow-up tool, and the model needs the id in hand to ever call them.
func TestExecuteTool_QueryMemory_FormatsNoteHitWithRefID(t *testing.T) {
	brain := &toolTestBrain{
		hybridHits: []db.MemoryHit{
			{Source: "note", Content: "Samara is my wife", RefID: 105},
		},
	}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "query_memory", map[string]any{"query": "Samara"})

	if !strings.Contains(result, "[note#105] Samara is my wife") {
		t.Errorf(`expected note hit formatted as "[note#105] ...", got: %q`, result)
	}
}

// TestExecuteTool_QueryMemory_TruncatesOverlongHitContent verifies query_memory formats hits through db.FormatHit like every other read path — a hit whose content exceeds the excerpt budget must come back truncated, not injected raw. Raw Activity Log summaries in production run tens of KB; an uncapped hit here can consume the entire tool-result byte budget by itself.
func TestExecuteTool_QueryMemory_TruncatesOverlongHitContent(t *testing.T) {
	overlong := strings.Repeat("x", 500) // well past db.maxEpisodeExcerpt (200 runes)
	brain := &toolTestBrain{
		hybridHits: []db.MemoryHit{
			{Source: "summary", Content: overlong},
		},
	}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "query_memory", map[string]any{"query": "anything"})

	if strings.Contains(result, overlong) {
		t.Fatalf("expected overlong hit content to be truncated, got full %d-char content in result", len(overlong))
	}
	if len(result) >= len(overlong) {
		t.Errorf("expected result shorter than the untruncated content (%d chars), got %d chars", len(overlong), len(result))
	}
}

// TestExecuteTool_QueryMemory_ZeroHits_ReturnsNoMatchesString verifies that zero hits from HybridSearch still returns a sensible "no memory matches"-style string rather than an empty string or a panic.
func TestExecuteTool_QueryMemory_ZeroHits_ReturnsNoMatchesString(t *testing.T) {
	brain := &toolTestBrain{hybridHits: nil}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "query_memory", map[string]any{"query": "nothing matches this"})

	if result == "" {
		t.Fatal("expected a non-empty sentinel string for zero hits, got empty string")
	}
	if !strings.Contains(strings.ToLower(result), "no memory") {
		t.Errorf(`expected a "no memory matches"-style string, got: %q`, result)
	}
}

// TestLiveTools_IncludesFunctionDeclarationsAndGoogleSearch verifies the Live API tool list carries both ORA's custom function tools AND Gemini's native GoogleSearch grounding tool — so Ora can look things up instead of answering from memory alone (see liveTools' doc comment in tools.go for the live-verification note).
func TestLiveTools_IncludesFunctionDeclarationsAndGoogleSearch(t *testing.T) {
	tools := liveTools()

	var hasFunctionDecls, hasGoogleSearch bool
	for _, tl := range tools {
		if len(tl.FunctionDeclarations) > 0 {
			hasFunctionDecls = true
		}
		if tl.GoogleSearch != nil {
			hasGoogleSearch = true
		}
	}
	if !hasFunctionDecls {
		t.Error("expected liveTools to include the custom FunctionDeclarations tool")
	}
	if !hasGoogleSearch {
		t.Error("expected liveTools to include the native GoogleSearch grounding tool")
	}
}

// TestToolDefinitions_AllNonBlocking verifies every function declaration is declared NON_BLOCKING. Left unset, the Live API treats a declaration as BLOCKING, which makes the model stop talking and stop listening for the whole duration of a tool call — a memory lookup that takes two seconds turns into two seconds of dead air on a voice call. NON_BLOCKING lets the model keep the conversation going while the result comes back out of band (see toolResponseScheduling for how the result is then folded in).
func TestToolDefinitions_AllNonBlocking(t *testing.T) {
	for _, tool := range toolDefinitions() {
		for _, fd := range tool.FunctionDeclarations {
			if fd.Behavior != genai.BehaviorNonBlocking {
				t.Errorf("tool %q has Behavior %q, want %q", fd.Name, fd.Behavior, genai.BehaviorNonBlocking)
			}
		}
	}
}

// --- save_note ---
//
// Nothing said IN CONVERSATION reached long-term memory before this tool existed: LogNote was only ever called from the TUI's /note slash command or the background screen-activity compiler, never from the live agent itself. save_note closes that gap.

func TestExecuteTool_SaveNote_Success_CallsLogNoteAndReturnsSaved(t *testing.T) {
	brain := &toolTestBrain{}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "save_note", map[string]any{"content": "user has a dentist appointment Friday"})

	if brain.loggedNoteContent != "user has a dentist appointment Friday" {
		t.Errorf("expected LogNote to receive the content, got %q", brain.loggedNoteContent)
	}
	if brain.loggedNoteKind != "fact" {
		t.Errorf(`expected LogNote to be called with kind "fact", got %q`, brain.loggedNoteKind)
	}
	if result != "saved" {
		t.Errorf(`expected result "saved", got %q`, result)
	}
}

// --- update_note ---
//
// Closes the gap where the model could save a misheard/wrong fact (save_note) but had no way to fix it in the same conversation — it could only apologize verbally while the bad note stayed in memory forever. Pairs with query_memory's "[note#N]" formatting: the model looks the note up, gets its id, then calls this.

func TestExecuteTool_UpdateNote_Success_CallsUpdateNoteAndReturnsConfirmation(t *testing.T) {
	brain := &toolTestBrain{}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "update_note", map[string]any{
		"id":      float64(105),
		"content": "Samara is my cat",
	})

	if brain.updatedNoteID != 105 {
		t.Errorf("expected UpdateNote to receive id 105, got %d", brain.updatedNoteID)
	}
	if brain.updatedNoteContent != "Samara is my cat" {
		t.Errorf("expected UpdateNote to receive the new content, got %q", brain.updatedNoteContent)
	}
	if result != "updated" {
		t.Errorf(`expected result "updated", got %q`, result)
	}
}

// --- delete_note ---

func TestExecuteTool_DeleteNote_Success_CallsDeleteNoteAndReturnsConfirmation(t *testing.T) {
	brain := &toolTestBrain{}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "delete_note", map[string]any{"id": float64(105)})

	if brain.deletedNoteID != 105 {
		t.Errorf("expected DeleteNote to receive id 105, got %d", brain.deletedNoteID)
	}
	if result != "deleted" {
		t.Errorf(`expected result "deleted", got %q`, result)
	}
}

// The note tools report a bad argument or a failed store write back to the model as an "error: ..." result instead of a silent no-op, and never touch the store on a bad argument.
func TestExecuteTool_NoteTools_BadArgsAndStoreFailures(t *testing.T) {
	cases := []struct {
		name  string
		tool  string
		args  map[string]any
		brain *toolTestBrain
	}{
		{"save_note without content", "save_note", map[string]any{}, &toolTestBrain{}},
		{"save_note with whitespace-only content", "save_note", map[string]any{"content": "   "}, &toolTestBrain{}},
		{"save_note when LogNote fails", "save_note", map[string]any{"content": "something"}, &toolTestBrain{logNoteErr: fmt.Errorf("db closed")}},
		{"update_note without id", "update_note", map[string]any{"content": "something"}, &toolTestBrain{}},
		{"update_note without content", "update_note", map[string]any{"id": float64(105)}, &toolTestBrain{}},
		{"update_note when UpdateNote fails", "update_note", map[string]any{"id": float64(105), "content": "something"}, &toolTestBrain{updateNoteErr: fmt.Errorf("db closed")}},
		{"delete_note without id", "delete_note", map[string]any{}, &toolTestBrain{}},
		{"delete_note when DeleteNote fails", "delete_note", map[string]any{"id": float64(105)}, &toolTestBrain{deleteNoteErr: fmt.Errorf("db closed")}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := NewAgent(nil, nil, tc.brain, nil, "FAKE_API_KEY")
			result := a.executeTool(context.Background(), tc.tool, tc.args)
			if !strings.HasPrefix(result, "error") {
				t.Errorf("expected an error result, got %q", result)
			}
			if tc.brain.logNoteErr == nil && tc.brain.loggedNoteContent != "" {
				t.Errorf("expected the store to be left alone on a bad argument, but LogNote got %q", tc.brain.loggedNoteContent)
			}
		})
	}
}

// --- exfiltration gap: read_file / read_clipboard HITL gating (F1b) ---

// TestIsSensitivePath is table-driven over the patterns read_file gates on: SSH/GPG/AWS credential dirs, .env, private key files (id_rsa/id_ed25519/*.pem/*.key), "credentials", "shadow", and ora's own IPC token — versus ordinary paths that should stay frictionless.
func TestIsSensitivePath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/home/user/.ssh/id_rsa", true},
		{"~/.ssh/config", true},
		{"/home/user/.gnupg/secring.gpg", true},
		{"/home/user/.aws/credentials", true},
		{"/home/user/project/.env", true},
		{"id_rsa", true},
		{"/home/user/.ssh/id_ed25519", true},
		{"/home/user/certs/server.pem", true},
		{"/home/user/keys/api.key", true},
		{"/etc/shadow", true},
		{"ora-db/ipc-token", true},
		{"/some/path/credentials.json", true},
		{"main.go", false},
		{"README.md", false},
		{"internal/agent/tools.go", false},
		{"/home/user/Documents/notes.txt", false},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			if got := isSensitivePath(tc.path); got != tc.want {
				t.Errorf("isSensitivePath(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestExecuteTool_ReadFile_SensitivePath_BlocksOnApproval verifies a sensitive path blocks on ToolApprovalChan instead of shipping its content straight to the model.
func TestExecuteTool_ReadFile_SensitivePath_BlocksOnApproval(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	tmpFile := filepath.Join(t.TempDir(), ".ssh", "id_rsa")
	if err := os.MkdirAll(filepath.Dir(tmpFile), 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(tmpFile, []byte("-----BEGIN PRIVATE KEY-----"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	done := make(chan string, 1)
	go func() {
		done <- a.executeTool(context.Background(), "read_file", map[string]any{"path": tmpFile})
	}()

	var req ToolRequest
	select {
	case req = <-a.ToolApprovalChan:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the sensitive read_file HITL approval request")
	}
	if !strings.Contains(req.Description, tmpFile) {
		t.Errorf("expected the approval description to mention the path, got %q", req.Description)
	}
	req.ResultChan <- "approved content"

	select {
	case got := <-done:
		if got != "approved content" {
			t.Errorf("expected the approved result to flow through, got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for executeTool to return after approval")
	}
}

// TestExecuteTool_ReadFile_NonSensitivePath_NoApproval verifies an ordinary path returns its content directly without ever touching ToolApprovalChan.
func TestExecuteTool_ReadFile_NonSensitivePath_NoApproval(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	tmpFile := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(tmpFile, []byte("just some notes"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	result := a.executeTool(context.Background(), "read_file", map[string]any{"path": tmpFile})

	if result != "just some notes" {
		t.Errorf("expected the file content returned directly, got %q", result)
	}
	select {
	case req := <-a.ToolApprovalChan:
		t.Fatalf("expected no HITL approval request for a non-sensitive path, got %+v", req)
	default:
	}
}

// TestExecuteTool_ReadClipboard_BlocksOnApproval verifies read_clipboard always requires approval, regardless of content — the clipboard can carry secrets a password manager just copied.
func TestExecuteTool_ReadClipboard_BlocksOnApproval(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")

	done := make(chan string, 1)
	go func() {
		done <- a.executeTool(context.Background(), "read_clipboard", map[string]any{})
	}()

	var req ToolRequest
	select {
	case req = <-a.ToolApprovalChan:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the read_clipboard HITL approval request")
	}
	if req.AllowKey != "read_clipboard" {
		t.Errorf("expected AllowKey %q, got %q", "read_clipboard", req.AllowKey)
	}
	req.ResultChan <- "clipboard was approved"

	select {
	case got := <-done:
		if got != "clipboard was approved" {
			t.Errorf("expected the approved result to flow through, got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for executeTool to return after approval")
	}
}

// TestExecuteTool_ReadClipboard_SessionAllowed_SkipsApproval verifies "Allow for session" (AllowedCmds keyed "read_clipboard") skips the HITL prompt on later calls within the same session.
func TestExecuteTool_ReadClipboard_SessionAllowed_SkipsApproval(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	a.AllowedCmds.Store("read_clipboard", true)

	done := make(chan string, 1)
	go func() {
		done <- a.executeTool(context.Background(), "read_clipboard", map[string]any{})
	}()

	select {
	case req := <-a.ToolApprovalChan:
		t.Fatalf("expected no HITL approval request once session-allowed, got %+v", req)
	case <-done:
		// executeTool returned without ever touching ToolApprovalChan — correct.
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for executeTool to return")
	}
}

// TestExecuteTool_GetRecent_TimestampsRenderInLocalTime verifies get_recent converts each episode's timestamp to the user's local zone, the same as the recall timeline path. Episodes are stored in UTC, so rendering the stored zone verbatim shows every time off by the UTC offset and can show the wrong date entirely.
func TestExecuteTool_GetRecent_TimestampsRenderInLocalTime(t *testing.T) {
	_, localOffset := time.Now().Local().Zone()
	fixedZone := time.FixedZone("FIXED", localOffset+3*3600)
	created := time.Date(2026, 7, 4, 8, 30, 0, 0, fixedZone)

	brain := &toolTestBrain{
		windowEpisodes: []db.Episode{
			{ID: 1, CreatedAt: created, App: "Mail", Title: "Inbox", ScreenText: "reading morning emails"},
		},
	}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "get_recent", map[string]any{"limit": float64(5)})

	wantLocal := created.In(time.Local).Format("Jan 2 15:04")
	wantStoredZone := created.Format("Jan 2 15:04")
	if !strings.Contains(result, wantLocal) {
		t.Errorf("expected get_recent to render the timestamp in local time (%s), got: %q", wantLocal, result)
	}
	if strings.Contains(result, wantStoredZone) {
		t.Errorf("get_recent rendered the timestamp in its stored zone (%s) instead of local time, got: %q", wantStoredZone, result)
	}
}

// TestFormatEpisodeTimeline_LeadsWithTheHumanHandle is taste criteria T1/T2: the row the model reads must put the thing the user was doing first and push the app and window title into a parenthetical, so the sentence it builds from the row is about the work rather than about the software. The old shape led with "LibreOffice Calc — portfolio_vulnerability_scores.xlsx", which is what got read out loud.
func TestFormatEpisodeTimeline_LeadsWithTheHumanHandle(t *testing.T) {
	at := time.Date(2026, 8, 28, 15, 4, 0, 0, time.Local)
	lines := formatEpisodeTimeline([]db.Episode{{
		CreatedAt:    at,
		App:          "mutter-x11-frames",
		Title:        "portfolio_vulnerability_scores.xlsx — LibreOffice Calc",
		UserActivity: "the vulnerability scoring",
		ScreenText:   "column J holds the score",
	}}, func(e db.Episode) string { return e.ScreenText })

	want := "[Aug 28 15:04] the vulnerability scoring (LibreOffice Calc, portfolio_vulnerability_scores.xlsx): column J holds the score"
	if len(lines) != 1 || lines[0] != want {
		t.Errorf("expected %q, got: %q", want, lines)
	}
}

// TestFormatEpisodeTimeline_FallsBackToTheAppName verifies a capture with no human handle still renders one row: the app name leads and only the window title stays in the parenthetical, rather than printing an empty handle or repeating the app twice.
func TestFormatEpisodeTimeline_FallsBackToTheAppName(t *testing.T) {
	at := time.Date(2026, 8, 28, 9, 15, 0, 0, time.Local)
	lines := formatEpisodeTimeline([]db.Episode{{
		CreatedAt:  at,
		App:        "Mail",
		Title:      "Inbox",
		ScreenText: "reading morning emails",
	}}, func(e db.Episode) string { return e.ScreenText })

	want := "[Aug 28 09:15] Mail (Inbox): reading morning emails"
	if len(lines) != 1 || lines[0] != want {
		t.Errorf("expected %q, got: %q", want, lines)
	}
}

// TestExecuteTool_ErrorsAreSaidInPlainWords covers every failure the model can provoke with bad arguments or a broken store: the result says what went wrong in words a person would use and keeps enough signal to retry, and no Go error text (a time-parse dump, a %!v verb, a type name) survives into it. A real session read "parsing time \"2 days ago\"" out loud to the user.
func TestExecuteTool_ErrorsAreSaidInPlainWords(t *testing.T) {
	goText := []string{"parsing time", "cannot parse", "%!", "0x", "ISO-8601", "RFC3339", "map[", "*errors", "got float64", "sql:"}
	cases := []struct {
		name  string
		tool  string
		args  map[string]any
		brain *toolTestBrain
	}{
		{"unreadable since", "recall", map[string]any{"since": "last tuesdayish"}, &toolTestBrain{}},
		{"unreadable query_memory since", "query_memory", map[string]any{"query": "riddler", "since": "sometime"}, &toolTestBrain{}},
		{"non-string since", "recall", map[string]any{"since": float64(2026)}, &toolTestBrain{}},
		{"reversed range", "recall", map[string]any{"since": "2026-08-28", "until": "2026-08-01"}, &toolTestBrain{}},
		{"store failure", "update_note", map[string]any{"id": float64(7), "content": "the corrected fact"}, &toolTestBrain{updateNoteErr: fmt.Errorf("no note with id 7")}},
		{"missing id", "delete_note", map[string]any{}, &toolTestBrain{}},
		{"unknown tool", "teleport", map[string]any{}, &toolTestBrain{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := NewAgent(nil, nil, tc.brain, nil, "FAKE_API_KEY")
			result := a.executeTool(context.Background(), tc.tool, tc.args)
			if !strings.HasPrefix(result, "error: ") {
				t.Fatalf(`expected an "error: ..." result, got %q`, result)
			}
			for _, bad := range goText {
				if strings.Contains(result, bad) {
					t.Errorf("Go error text %q reached the model: %q", bad, result)
				}
			}
		})
	}
}

// TestExecuteTool_BadDate_SaysWhichDatesWork verifies the one date error phrasing names the forms that do work, so the model can fix the argument in the same turn instead of ending the turn on a failure.
func TestExecuteTool_BadDate_SaysWhichDatesWork(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "recall", map[string]any{"since": "last tuesdayish"})

	for _, want := range []string{"real date", "today", "yesterday", "2026-07-05"} {
		if !strings.Contains(result, want) {
			t.Errorf("expected the date error to mention %q so the model can retry, got: %q", want, result)
		}
	}
}

// TestExecuteTool_Recall_UnknownArgument_ReturnsError verifies that an argument recall doesn't have (the real trace called recall with "query", a query_memory parameter) is rejected by name instead of falling through to the timeline branch, where since defaults to the start of today and the model gets a confidently wrong answer for a question that had nothing to do with today.
func TestExecuteTool_Recall_UnknownArgument_ReturnsError(t *testing.T) {
	brain := &toolTestBrain{
		windowEpisodes: []db.Episode{{App: "Brave", Title: "YouTube", ScreenText: "unrelated", CreatedAt: time.Now()}},
	}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "recall", map[string]any{"query": "Anime Platform Authentication"})

	if !strings.HasPrefix(result, "error") {
		t.Fatalf(`expected an "error: ..." result for an unknown recall argument, got %q`, result)
	}
	if !strings.Contains(result, "query") {
		t.Errorf("expected the error to name the offending argument, got %q", result)
	}
	for _, valid := range []string{"subject", "since", "until", "app"} {
		if !strings.Contains(result, valid) {
			t.Errorf("expected the error to list valid argument %q so the model can self-correct, got %q", valid, result)
		}
	}
	if strings.Contains(result, "YouTube") {
		t.Errorf("expected no timeline results to leak through for a rejected call, got %q", result)
	}
}

// TestExecuteTool_QueryMemory_UnknownArgument_ReturnsError verifies query_memory rejects invented parameters the same way recall does, rather than ignoring them.
func TestExecuteTool_QueryMemory_UnknownArgument_ReturnsError(t *testing.T) {
	brain := &toolTestBrain{hybridHits: []db.MemoryHit{{Source: "note", Content: "a note"}}}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "query_memory", map[string]any{"query": "riddler", "subject": "riddler"})

	if !strings.HasPrefix(result, "error") {
		t.Fatalf(`expected an "error: ..." result for an unknown query_memory argument, got %q`, result)
	}
	if !strings.Contains(result, "subject") {
		t.Errorf("expected the error to name the offending argument, got %q", result)
	}
}

// TestParseInstant_ZonelessDateTime verifies a datetime with no zone offset ("2026-08-20T00:00:00", which the model emits often) parses as local time instead of erroring out of the whole recall call.
func TestParseInstant_ZonelessDateTime(t *testing.T) {
	now := time.Date(2026, 8, 28, 9, 0, 0, 0, time.Local)
	got, err := parseInstant("2026-08-20T14:30:00", now, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := time.Date(2026, 8, 20, 14, 30, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Errorf("parseInstant = %v, want %v", got, want)
	}
}

// TestParseInstant_TodayAndYesterday verifies the two temporal words the user actually says resolve against now, so "what did I do today" filters by date instead of lexically matching the word "today" (which is how "India Today" articles came back as the answer).
func TestParseInstant_TodayAndYesterday(t *testing.T) {
	now := time.Date(2026, 8, 28, 9, 0, 0, 0, time.Local)
	cases := []struct {
		in       string
		endOfDay bool
		want     time.Time
	}{
		{"today", false, time.Date(2026, 8, 28, 0, 0, 0, 0, time.Local)},
		{"today", true, time.Date(2026, 8, 28, 23, 59, 59, 0, time.Local)},
		{"yesterday", false, time.Date(2026, 8, 27, 0, 0, 0, 0, time.Local)},
		{"Yesterday", true, time.Date(2026, 8, 27, 23, 59, 59, 0, time.Local)},
	}
	for _, c := range cases {
		got, err := parseInstant(c.in, now, c.endOfDay)
		if err != nil {
			t.Fatalf("parseInstant(%q, endOfDay=%v): unexpected error: %v", c.in, c.endOfDay, err)
		}
		if !got.Equal(c.want) {
			t.Errorf("parseInstant(%q, endOfDay=%v) = %v, want %v", c.in, c.endOfDay, got, c.want)
		}
	}
}

// TestExecuteTool_QueryMemory_SinceFiltersHitsByCreatedAt verifies query_memory accepts a since/until window and drops hits outside it. Without this, "what did I read today" is a pure lexical search for the word "today" and an "India Today" article from last month outranks anything that actually happened today.
func TestExecuteTool_QueryMemory_SinceFiltersHitsByCreatedAt(t *testing.T) {
	now := time.Now()
	brain := &toolTestBrain{hybridHits: []db.MemoryHit{
		{Source: "episode", Content: "India Today front page", RefID: 1, CreatedAt: now.AddDate(0, 0, -30)},
		{Source: "episode", Content: "reviewing the retrieval audit", RefID: 2, CreatedAt: now},
	}}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "query_memory", map[string]any{"query": "today", "since": "today"})

	if strings.Contains(result, "India Today") {
		t.Errorf("expected the month-old hit to be dropped by the since filter, got %q", result)
	}
	if !strings.Contains(result, "reviewing the retrieval audit") {
		t.Errorf("expected today's hit to survive the since filter, got %q", result)
	}
}

// TestExecuteTool_QueryMemory_TimeFilterOverfetches verifies a since/until window asks the store for more than the final 10 hits — the ranking that produces those 10 knows nothing about the time window, so filtering the top 10 after the fact would usually leave nothing.
func TestExecuteTool_QueryMemory_TimeFilterOverfetches(t *testing.T) {
	brain := &toolTestBrain{hybridHits: []db.MemoryHit{{Source: "episode", Content: "x", CreatedAt: time.Now()}}}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	a.executeTool(context.Background(), "query_memory", map[string]any{"query": "anything", "since": "today"})
	timed := brain.capturedHybridLimit
	a.executeTool(context.Background(), "query_memory", map[string]any{"query": "anything"})
	untimed := brain.capturedHybridLimit

	if timed <= untimed {
		t.Errorf("expected a time-filtered query to over-fetch (got limit %d) versus the plain limit %d", timed, untimed)
	}
}

// TestExecuteTool_QueryMemory_BadDate_ReturnsError verifies an unparseable since/until is reported instead of ignored.
func TestExecuteTool_QueryMemory_BadDate_ReturnsError(t *testing.T) {
	brain := &toolTestBrain{hybridHits: []db.MemoryHit{{Source: "note", Content: "a note"}}}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "query_memory", map[string]any{"query": "riddler", "since": "last tuesdayish"})

	if !strings.HasPrefix(result, "error") {
		t.Errorf(`expected an "error: ..." result for an unparseable since, got %q`, result)
	}
}

// TestExecuteTool_UpdateNote_StoreError_IsReportedNotSwallowed verifies update_note reports a failed write instead of answering "updated" — reporting a correction that never landed leaves the wrong fact in memory and tells the model the opposite. The store's own error text stays out of the result; what the model gets is the failure plus what to do about it.
func TestExecuteTool_UpdateNote_StoreError_IsReportedNotSwallowed(t *testing.T) {
	brain := &toolTestBrain{updateNoteErr: fmt.Errorf("no note with id 7")}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "update_note", map[string]any{"id": float64(7), "content": "the corrected fact"})

	if result == "updated" {
		t.Fatal(`expected update_note to report the store error, got "updated"`)
	}
	if !strings.HasPrefix(result, "error") {
		t.Errorf(`expected an "error: ..." result, got %q`, result)
	}
	if !strings.Contains(result, "query_memory") {
		t.Errorf("expected the failure to tell the model how to get the right id, got %q", result)
	}
}

// TestExecuteTool_Recall_SkipsIdleCaptures is bug 4: the tracker writes the literal string "Unknown" for an app and title it could not read, and a night of idle captures turned 25 of 40 recall rows into "Unknown — Unknown: Unknown". Those rows carry no information and crowd out the ones that do, so they are dropped and counted instead.
func TestExecuteTool_Recall_SkipsIdleCaptures(t *testing.T) {
	base := time.Date(2026, 8, 28, 1, 0, 0, 0, time.Local)
	brain := &toolTestBrain{
		windowEpisodes: []db.Episode{
			{ID: 1, CreatedAt: base, App: "Unknown", Title: "Unknown", ScreenText: "Unknown"},
			{ID: 2, CreatedAt: base.Add(time.Minute), App: "Unknown", Title: "Unknown", ScreenText: ""},
			{ID: 3, CreatedAt: base.Add(2 * time.Minute), App: "", Title: "", ScreenText: ""},
			{ID: 4, CreatedAt: base.Add(3 * time.Minute), App: "LibreOffice Calc", Title: "portfolio.xlsx", ScreenText: "editing a spreadsheet"},
		},
	}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "recall", map[string]any{"since": "2026-08-28", "until": "2026-08-28"})

	if strings.Contains(result, "Unknown — Unknown") {
		t.Errorf("expected idle captures dropped from the timeline, got: %q", result)
	}
	if !strings.Contains(result, "portfolio.xlsx") {
		t.Errorf("expected the real episode kept, got: %q", result)
	}
	if !strings.Contains(result, "nothing on screen for 2m") {
		t.Errorf("expected the dropped captures said as a stretch of time, got: %q", result)
	}
	if strings.Contains(result, "omitted") {
		t.Errorf("expected no capture-count bookkeeping in the timeline, got: %q", result)
	}
}

// TestExecuteTool_Recall_IdleGapNamesTheSpan is taste criterion T9: a long idle stretch is time the user can picture, not a count of rows the tool threw away. "(19 idle omitted)" is what made a real user answer "are you serious?".
func TestExecuteTool_Recall_IdleGapNamesTheSpan(t *testing.T) {
	base := time.Date(2026, 8, 28, 1, 0, 0, 0, time.Local)
	brain := &toolTestBrain{
		windowEpisodes: []db.Episode{
			{ID: 1, CreatedAt: base.Add(2*time.Hour + 10*time.Minute), App: "Unknown", Title: "Unknown", ScreenText: "Unknown"},
			{ID: 2, CreatedAt: base.Add(time.Hour), App: "Unknown", Title: "Unknown", ScreenText: ""},
			{ID: 3, CreatedAt: base, App: "", Title: "", ScreenText: ""},
		},
	}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "recall", map[string]any{"since": "2026-08-28", "until": "2026-08-28"})

	if !strings.Contains(result, "nothing on screen for 2h 10m") {
		t.Errorf("expected the idle run reported as its own span, got: %q", result)
	}
}

// TestExecuteTool_Recall_AllIdle_StillReportsTheGap verifies a window with nothing but idle captures says so once, rather than returning an empty string the model has to guess at.
func TestExecuteTool_Recall_AllIdle_StillReportsTheGap(t *testing.T) {
	base := time.Date(2026, 8, 28, 1, 0, 0, 0, time.Local)
	brain := &toolTestBrain{
		windowEpisodes: []db.Episode{
			{ID: 1, CreatedAt: base, App: "Unknown", Title: "Unknown", ScreenText: "Unknown"},
			{ID: 2, CreatedAt: base.Add(time.Minute), App: "Unknown", Title: "Unknown", ScreenText: "Unknown"},
		},
	}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "recall", map[string]any{"since": "2026-08-28", "until": "2026-08-28"})

	if !strings.Contains(result, "nothing on screen for 1m") {
		t.Errorf("expected the all-idle window to report the gap once, in plain words, got: %q", result)
	}
}

// TestExecuteTool_GetRecent_SkipsIdleCaptures verifies get_recent drops the same dead rows — it formats the identical line shape from the identical episodes, so fixing only recall would leave "what was I just doing" answering with "Unknown — Unknown: Unknown".
func TestExecuteTool_GetRecent_SkipsIdleCaptures(t *testing.T) {
	base := time.Date(2026, 8, 28, 1, 0, 0, 0, time.Local)
	brain := &toolTestBrain{
		windowEpisodes: []db.Episode{
			{ID: 1, CreatedAt: base, App: "Unknown", Title: "Unknown", ScreenText: "Unknown"},
			{ID: 2, CreatedAt: base.Add(time.Minute), App: "gnome-terminal-server", Title: "user@host", ScreenText: "go test ./..."},
		},
	}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "get_recent", map[string]any{})

	if strings.Contains(result, "Unknown — Unknown") {
		t.Errorf("expected idle captures dropped from get_recent, got: %q", result)
	}
	if !strings.Contains(result, "go test") {
		t.Errorf("expected the real episode kept, got: %q", result)
	}
	if !strings.Contains(result, "nothing on screen for") {
		t.Errorf("expected the dropped capture accounted for in plain words, got: %q", result)
	}
}

// TestExecuteTool_Recall_CollapsesWhitespaceInExcerpt covers the real shape a browser capture has: "choosing a profile\nWho's watching?\nm\nKids\nAdd\nEdit" arrived in a production recall result as six lines inside what is supposed to be one timeline row, which both breaks the line format the model is reading and spends the rune cap on layout instead of content.
func TestExecuteTool_Recall_CollapsesWhitespaceInExcerpt(t *testing.T) {
	base := time.Date(2026, 8, 27, 23, 17, 0, 0, time.Local)
	brain := &toolTestBrain{
		windowEpisodes: []db.Episode{
			{ID: 1, CreatedAt: base, App: "Brave Browser", Title: "JioHotstar - Brave", ScreenText: "choosing a profile\nWho's watching?\nm\n\tKids\nAdd\nEdit"},
		},
	}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "recall", map[string]any{"since": "2026-08-27", "until": "2026-08-27"})

	if strings.Count(result, "\n") != 0 {
		t.Errorf("expected one timeline row on one line, got: %q", result)
	}
	if !strings.Contains(result, "choosing a profile Who's watching? m Kids Add Edit") {
		t.Errorf("expected the excerpt collapsed to single spaces, got: %q", result)
	}
}

// TestExecuteTool_Recall_WrapperProcessNamesRealApp covers the mutter-x11-frames rows in production: the compositor owns the window, so the app column said "mutter-x11-frames" while the title carried the actual program ("portfolio_vulnerability_scores.xlsx — LibreOffice Calc"). The model has to be able to say "LibreOffice Calc".
func TestExecuteTool_Recall_WrapperProcessNamesRealApp(t *testing.T) {
	base := time.Date(2026, 8, 28, 10, 0, 0, 0, time.Local)
	brain := &toolTestBrain{
		windowEpisodes: []db.Episode{
			{ID: 1, CreatedAt: base, App: "mutter-x11-frames", Title: "portfolio_vulnerability_scores.xlsx — LibreOffice Calc", ScreenText: "vulnerability scores"},
		},
	}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "recall", map[string]any{"since": "2026-08-28", "until": "2026-08-28"})

	if strings.Contains(result, "mutter-x11-frames") {
		t.Errorf("expected the compositor process name kept out of the answer, got: %q", result)
	}
	if !strings.Contains(result, "LibreOffice Calc (portfolio_vulnerability_scores.xlsx)") {
		t.Errorf("expected the real program named with the file beside it, got: %q", result)
	}
}

// TestExecuteTool_Recall_RollsConsecutiveSameWindowRows covers the three identical "Family Guy - JioHotstar - Brave" rows a production recall returned: one window the user sat in for a stretch, printed three times with three near-identical excerpts. One line carrying how long it lasted says the same thing in a third of the tokens.
func TestExecuteTool_Recall_RollsConsecutiveSameWindowRows(t *testing.T) {
	base := time.Date(2026, 8, 27, 23, 17, 0, 0, time.Local)
	brain := &toolTestBrain{
		windowEpisodes: []db.Episode{
			{ID: 3, CreatedAt: base.Add(64 * time.Minute), App: "mutter-x11-frames", Title: "portfolio_vulnerability_scores.xlsx — LibreOffice Calc", ScreenText: "vulnerability scores"},
			{ID: 2, CreatedAt: base.Add(30 * time.Minute), App: "mutter-x11-frames", Title: "portfolio_vulnerability_scores.xlsx — LibreOffice Calc", ScreenText: "vulnerability scores"},
			{ID: 1, CreatedAt: base, App: "mutter-x11-frames", Title: "portfolio_vulnerability_scores.xlsx — LibreOffice Calc", ScreenText: "vulnerability scores"},
		},
	}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "recall", map[string]any{"since": "2026-08-27", "until": "2026-08-28"})

	if lines := strings.Count(result, "\n") + 1; lines != 1 {
		t.Errorf("expected the three captures of one window rolled into one line, got %d lines: %q", lines, result)
	}
	if !strings.Contains(result, "1h04m LibreOffice Calc (portfolio_vulnerability_scores.xlsx): vulnerability scores") {
		t.Errorf("expected the rolled line to carry how long that window was up, got: %q", result)
	}
}

// TestExecuteTool_Recall_SubjectWithDates_AnswersAndSaysTheWindowWasIgnored covers a call the model made twice in one session: recall with both a subject and a since. Erroring taught it nothing and cost the turn; the subject answer plus a note saying the window was not applied is the answer it was after.
func TestExecuteTool_Recall_SubjectWithDates_AnswersAndSaysTheWindowWasIgnored(t *testing.T) {
	brain := &toolTestBrain{subjectRecall: []string{"[thread] DeepSeek — studying post-training"}}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	for _, args := range []map[string]any{
		{"subject": "DeepSeek", "since": "2026-08-20"},
		{"subject": "DeepSeek", "until": "2026-08-20"},
	} {
		result := a.executeTool(context.Background(), "recall", args)
		if strings.HasPrefix(result, "error") {
			t.Errorf("expected an answer rather than an error for %v, got %q", args, result)
		}
		if !strings.Contains(result, "[thread] DeepSeek") {
			t.Errorf("expected the subject recall to still answer for %v, got %q", args, result)
		}
		if !strings.Contains(result, "subject recall ignores the date window") {
			t.Errorf("expected the ignored date window called out for %v, got %q", args, result)
		}
	}
}

// TestParseInstant_DaysAgo verifies the phrase the model passes straight through from speech ("2 days ago") resolves to the start of that day instead of failing the whole call with a Go time-parse error the user then hears out loud.
func TestParseInstant_DaysAgo(t *testing.T) {
	now := time.Date(2026, 8, 28, 9, 0, 0, 0, time.Local)
	got, err := parseInstant("2 days ago", now, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := time.Date(2026, 8, 26, 0, 0, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Errorf("parseInstant(%q) = %v, want %v", "2 days ago", got, want)
	}
	if _, err := parseInstant("1 day ago", now, false); err != nil {
		t.Errorf("expected the singular form to parse too, got %v", err)
	}
}

// TestExecuteTool_QueryMemory_FiltersEmptiedANonEmptySet_SaysSo verifies the model can tell "the store has nothing about this" apart from "the app and date filters removed everything I found" — answering "no matches" to the second is how a real question about episodes watched today got a flat no while the data sat in the store.
func TestExecuteTool_QueryMemory_FiltersEmptiedANonEmptySet_SaysSo(t *testing.T) {
	now := time.Now()
	brain := &toolTestBrain{hybridHits: []db.MemoryHit{
		{Source: "episode", App: "Brave Browser", Title: "Suits", Content: "watching", CreatedAt: now},
		{Source: "episode", App: "Brave Browser", Title: "Suits", Content: "watching", CreatedAt: now},
	}}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "query_memory", map[string]any{"query": "episodes", "app": "slack"})

	if result == "no memory matches" {
		t.Fatalf("expected the emptied-by-filter case distinguished from a genuinely empty store, got: %q", result)
	}
	if !strings.Contains(result, "2 matches") || !strings.Contains(result, "none in slack") {
		t.Errorf("expected a count of what was found and which filter removed it, got: %q", result)
	}
}

// TestExecuteTool_GetRecent_OverFetchesSoIdleRowsDoNotEatTheLimit verifies asking for N recent moments returns up to N real ones: the store's newest rows are routinely idle captures, and fetching exactly N then dropping the idle ones is how "what did I do in the last six hours" came back as one row plus "(19 idle omitted)".
func TestExecuteTool_GetRecent_OverFetchesSoIdleRowsDoNotEatTheLimit(t *testing.T) {
	base := time.Date(2026, 8, 28, 3, 0, 0, 0, time.Local)
	var episodes []db.Episode
	for i := range 4 {
		episodes = append(episodes, db.Episode{ID: int64(i + 1), CreatedAt: base.Add(time.Duration(-i) * time.Minute), App: "Unknown", Title: "Unknown", ScreenText: "Unknown"})
	}
	for i := range 3 {
		episodes = append(episodes, db.Episode{ID: int64(i + 10), CreatedAt: base.Add(time.Duration(-10-i) * time.Minute), App: "Brave Browser", Title: fmt.Sprintf("page %d - Brave", i), ScreenText: "real content"})
	}
	brain := &toolTestBrain{windowEpisodes: episodes}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "get_recent", map[string]any{"limit": float64(2)})

	real := 0
	for _, line := range strings.Split(result, "\n") {
		if !strings.HasPrefix(line, idleGapPrefix) {
			real++
		}
	}
	if real != 2 {
		t.Errorf("expected 2 real moments for limit 2, got %d: %q", real, result)
	}
	if brain.capturedEpisodeQuery.Limit <= 2 {
		t.Errorf("expected get_recent to over-fetch past the limit so idle rows can be dropped, asked for %d", brain.capturedEpisodeQuery.Limit)
	}
}

// TestExecuteTool_FixThread covers the repair the model could not make in a real session: it recognised that a thread's summary had merged two unrelated things, and every write tool it had pointed at the notes table. fix_thread rewrites the thread's own summary from the id the "[thread#N]" hit carries.
func TestExecuteTool_FixThread(t *testing.T) {
	brain := &toolTestBrain{}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "fix_thread", map[string]any{
		"id":         float64(19),
		"correction": "mf x mdev is a Google Meet call, unrelated to the ORA work",
	})

	if result != "fixed" {
		t.Fatalf("fix_thread = %q, want %q", result, "fixed")
	}
	if brain.updatedThreadID != 19 || brain.updatedThreadState != "mf x mdev is a Google Meet call, unrelated to the ORA work" {
		t.Errorf("thread %d updated to %q, want the correction against thread 19", brain.updatedThreadID, brain.updatedThreadState)
	}
}

// TestExecuteTool_FixThread_MissingArgs verifies a call with no id or an empty correction says what is missing rather than silently reporting a fix.
func TestExecuteTool_FixThread_MissingArgs(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "FAKE_API_KEY")

	for _, args := range []map[string]any{
		{"correction": "the meeting was on Google Meet"},
		{"id": float64(19)},
	} {
		if result := a.executeTool(context.Background(), "fix_thread", args); !strings.HasPrefix(result, "error") {
			t.Errorf("fix_thread(%v) = %q, want an error", args, result)
		}
	}
}

// TestToolDefinitions_DeclaresFixThread verifies the tool is actually offered to the model — the repair it enables is worthless if only executeTool knows about it.
func TestToolDefinitions_DeclaresFixThread(t *testing.T) {
	for _, tool := range toolDefinitions() {
		for _, decl := range tool.FunctionDeclarations {
			if decl.Name == "fix_thread" {
				return
			}
		}
	}
	t.Error("toolDefinitions does not declare fix_thread")
}
