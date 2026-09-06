package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"ora/internal/act"
	"ora/internal/db"
	"ora/internal/memory"
	oratext "ora/internal/text"
	"ora/internal/tracker"
	"ora/internal/window"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"google.golang.org/genai"
)

// toolTestBrain is a minimal ContextReader mock used to exercise executeTool's query_memory case. It lives in an internal (package agent, not agent_test) test file because executeTool is unexported.
type toolTestBrain struct {
	notes           []db.Note
	threadEpisodes  []db.Episode
	openActions     []memory.ActionItem
	openActionsErr  error
	windowEpisodes  []db.Episode
	windowSummaries []db.WindowSummary
	subjectRecall   []string

	// searchMemoryResult/searchMemoryCalledFocus back SearchMemory — used by buildHandshakeContext's tests (F2) to verify the [working]-buffer focus signal actually drives a SearchMemory call.
	searchMemoryResult      []db.MemoryHit
	searchMemoryCalledFocus string

	// hybridHits/capturedDomain back HybridSearchWindow: configurable return value plus a capture of the domainFilter arg, same field-per-method style as the rest of this mock. A call with a nonzero window returns windowedHybridHits instead, so a test can simulate "the topic exists, but not inside the window" without a real store.
	hybridHits         []db.MemoryHit
	windowedHybridHits []db.MemoryHit
	capturedDomain     string
	// capturedHybridLimit/capturedSince/capturedUntil record what HybridSearchWindow was asked for, so tests can assert query_memory hands the window to the store instead of post-filtering.
	capturedHybridLimit          int
	capturedSince, capturedUntil time.Time

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

	// updatedNoteID/updatedNoteContent capture UpdateNote's args for the revise tool's note-content tests; updateNoteErr forces it to fail.
	updatedNoteID      int64
	updatedNoteContent string
	updateNoteErr      error

	// actionID/actionStatus/actionPriority capture SetActionStatus/SetActionPriority's args for the revise tool's action-state tests; actionErr forces both to fail.
	actionID       int64
	actionStatus   string
	actionPriority string
	actionErr      error
	// actionText captures SetActionText's arg; isAction says whether the id the test uses names an action item at all — false, the zero value, makes SetActionText report db.ErrNotActionItem the way the store does for a plain note, so revise falls back to UpdateNote.
	actionText string
	isAction   bool
	// updatedThreadID/updatedThreadState capture UpdateThreadState's args for the revise tool's thread tests; updateThreadErr forces it to fail.
	updatedThreadID    int64
	updatedThreadState string
	updateThreadErr    error

	// personal/personalErr back the personal_context tool's tests: an in-memory subject->content map with the same edit-in-place semantics as the store, and a forced failure.
	personal    map[string]string
	personalErr error

	// deletedNoteID captures DeleteNote's arg for the revise tool's remove tests; deleteNoteErr forces it to fail.
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

	// capturedQuery/capturedRowCap record QueryStore's args; queryStoreResult/queryStoreErr back its return, for the query_store tool's tests.
	capturedQuery    string
	capturedRowCap   int
	queryStoreResult string
	queryStoreErr    error
}

func (b *toolTestBrain) GetImplicitContext(ctx context.Context) ([]string, error) {
	return b.implicitContext, nil
}
func (b *toolTestBrain) SearchMemory(ctx context.Context, query string) ([]db.MemoryHit, error) {
	b.searchMemoryCalledFocus = query
	return b.searchMemoryResult, nil
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
func (b *toolTestBrain) GetNotes(ctx context.Context) ([]db.Note, error) { return b.notes, nil }
func (b *toolTestBrain) NotesOfKindSince(ctx context.Context, kind string, since time.Time) ([]db.Note, error) {
	var out []db.Note
	for i := len(b.notes) - 1; i >= 0; i-- {
		n := b.notes[i]
		if n.Kind == kind && (since.IsZero() || !n.CreatedAt.Before(since)) {
			out = append(out, n)
		}
	}
	return out, nil
}
func (b *toolTestBrain) PersonalContext(ctx context.Context) ([]db.PersonalEntry, error) {
	if b.personalErr != nil {
		return nil, b.personalErr
	}
	var out []db.PersonalEntry
	for _, subject := range slices.Sorted(maps.Keys(b.personal)) {
		out = append(out, db.PersonalEntry{Subject: subject, Content: b.personal[subject]})
	}
	return out, nil
}
func (b *toolTestBrain) SetPersonalContext(ctx context.Context, subject, content string) error {
	if b.personalErr != nil {
		return b.personalErr
	}
	if b.personal == nil {
		b.personal = map[string]string{}
	}
	b.personal[subject] = content
	return nil
}
func (b *toolTestBrain) DeletePersonalContext(ctx context.Context, subject string) error {
	if b.personalErr != nil {
		return b.personalErr
	}
	delete(b.personal, subject)
	return nil
}
func (b *toolTestBrain) UpdateNote(ctx context.Context, id int64, content string) error {
	b.updatedNoteID = id
	b.updatedNoteContent = content
	return b.updateNoteErr
}
func (b *toolTestBrain) EpisodesForThread(ctx context.Context, threadID int64, limit int) ([]db.Episode, error) {
	return b.threadEpisodes, nil
}

func (b *toolTestBrain) OpenActionItems(ctx context.Context) ([]memory.ActionItem, error) {
	return b.openActions, b.openActionsErr
}

func (b *toolTestBrain) SetActionStatus(ctx context.Context, id int64, status string) error {
	b.actionID, b.actionStatus = id, status
	return b.actionErr
}

func (b *toolTestBrain) SetActionText(ctx context.Context, id int64, text string) error {
	if !b.isAction {
		return fmt.Errorf("no action item with id %d: %w", id, db.ErrNotActionItem)
	}
	b.actionID, b.actionText = id, text
	return b.actionErr
}

func (b *toolTestBrain) SetActionPriority(ctx context.Context, id int64, priority string) error {
	b.actionID, b.actionPriority = id, priority
	return b.actionErr
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
func (b *toolTestBrain) SummaryTimeline(ctx context.Context, since, until time.Time) ([]db.WindowSummary, error) {
	return b.windowSummaries, nil
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

// HybridSearchWindow records its arguments and returns hybridHits for an unwindowed call, windowedHybridHits when a window is set — so tests can assert executeTool's "query_memory" case wires domain and the time window through correctly.
func (b *toolTestBrain) HybridSearchWindow(ctx context.Context, query, domainFilter string, since, until time.Time, limit int) ([]db.MemoryHit, error) {
	b.capturedDomain = domainFilter
	b.capturedHybridLimit = limit
	b.capturedSince, b.capturedUntil = since, until
	if !since.IsZero() || !until.IsZero() {
		return b.windowedHybridHits, nil
	}
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

func (b *toolTestBrain) QueryStore(ctx context.Context, query string, rowCap int) (string, error) {
	b.capturedQuery = query
	b.capturedRowCap = rowCap
	return b.queryStoreResult, b.queryStoreErr
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

// TestToolDefinitions_DeclaresRevise verifies revise is declared to the model (not just wired in executeTool), with ref required and content/state/remove all optional — the single repair tool has to be offered before it can replace the four it stands in for.
func TestToolDefinitions_DeclaresRevise(t *testing.T) {
	var revise *genai.FunctionDeclaration
	for _, tool := range toolDefinitions() {
		for _, fd := range tool.FunctionDeclarations {
			if fd.Name == "revise" {
				revise = fd
			}
		}
	}
	if revise == nil {
		t.Fatal("revise tool declaration not found")
	}
	for _, name := range []string{"ref", "content", "state", "remove"} {
		if _, ok := revise.Parameters.Properties[name]; !ok {
			t.Errorf("expected revise to declare a %q parameter", name)
		}
	}
	if !slices.Contains(revise.Parameters.Required, "ref") {
		t.Errorf("expected revise to require ref, got required=%v", revise.Parameters.Required)
	}
	if slices.Contains(revise.Parameters.Required, "content") || slices.Contains(revise.Parameters.Required, "state") || slices.Contains(revise.Parameters.Required, "remove") {
		t.Errorf("expected only ref required, got required=%v", revise.Parameters.Required)
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

// TestExecuteTool_Recall_TimelinePath verifies that calling the "recall" tool with since/until args (and no "subject") surfaces the brain's canned timeline (ListEpisodes), formatted chronologically as "[HH:MM] app — title: ...".
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

// TestExecuteTool_QueryMemory_FormatsNoteHitWithRefID verifies note hits carry their ref_id in the surfaced line ("[note#105] ..." instead of just "[note] ..."), unlike every other source — notes are the only source with a revise follow-up tool, and the model needs the id in hand to ever call it.
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
	overlong := strings.Repeat("x", 2000) // well past db's excerpt budget for a summary (maxSummaryExcerpt, 700 runes)
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
	tools := liveToolsFor("gemini-2.5-flash-native-audio-preview-12-2025")

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

// --- revise ---
//
// Closes the gap where the model could save a misheard/wrong fact (save_note) but had no way to fix it in the same conversation — it could only apologize verbally while the bad note stayed in memory forever. One tool now covers what update_note, delete_note, update_action and fix_thread used to: it dispatches on the ref's own kind ("note#N" or "thread#N") to the same store calls those four used.

// TestExecuteTool_Revise_Note covers the three things a "note#N" ref can do: correct its content, set an action item's state (rewriting only the "[state/priority]" prefix via SetActionStatus, same as the old update_action), and remove it outright.
func TestExecuteTool_Revise_Note(t *testing.T) {
	t.Run("content", func(t *testing.T) {
		brain := &toolTestBrain{}
		a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")
		result := a.executeTool(context.Background(), "revise", map[string]any{"ref": "note#105", "content": "Samara is my cat"})
		if brain.updatedNoteID != 105 || brain.updatedNoteContent != "Samara is my cat" {
			t.Errorf("expected UpdateNote(105, ...), got id=%d content=%q", brain.updatedNoteID, brain.updatedNoteContent)
		}
		if result != "updated" {
			t.Errorf(`result = %q, want "updated"`, result)
		}
	})
	t.Run("state", func(t *testing.T) {
		brain := &toolTestBrain{}
		a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")
		result := a.executeTool(context.Background(), "revise", map[string]any{"ref": "[note#7]", "state": "done"})
		if brain.actionID != 7 || brain.actionStatus != "done" {
			t.Errorf("expected SetActionStatus(7, done), got id=%d status=%q", brain.actionID, brain.actionStatus)
		}
		if result != "updated" {
			t.Errorf(`result = %q, want "updated"`, result)
		}
		if bad := a.executeTool(context.Background(), "revise", map[string]any{"ref": "note#7", "state": "finished"}); !strings.HasPrefix(bad, "error") {
			t.Errorf("expected an invalid state to be refused, got %q", bad)
		}
	})
	t.Run("remove", func(t *testing.T) {
		brain := &toolTestBrain{}
		a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")
		result := a.executeTool(context.Background(), "revise", map[string]any{"ref": "note#105", "remove": true})
		if brain.deletedNoteID != 105 {
			t.Errorf("expected DeleteNote(105), got %d", brain.deletedNoteID)
		}
		if result != "deleted" {
			t.Errorf(`result = %q, want "deleted"`, result)
		}
	})
	// An id the model invented names nothing, and the store says so: the model must be told the note was never there, not that its removal succeeded, or the user hears "deleted" about a note that is still on file.
	t.Run("remove an id that names nothing", func(t *testing.T) {
		brain := &toolTestBrain{deleteNoteErr: fmt.Errorf("no note with id 4242")}
		a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")
		result := a.executeTool(context.Background(), "revise", map[string]any{"ref": "note#4242", "remove": true})
		if !strings.HasPrefix(result, "error") || !strings.Contains(result, "nothing was there") {
			t.Errorf("result = %q, want an error saying nothing was there", result)
		}
	})
	// An action item's content is a rendered "[state/priority] Owner — work (Meeting, date)" line, so correcting its work text goes through SetActionText, which re-renders it. Overwriting the whole line through UpdateNote would strip the prefix, ParseAction would stop reading the row, and the task would vanish from the brief and the Tasks screen.
	t.Run("content on an action item", func(t *testing.T) {
		brain := &toolTestBrain{isAction: true}
		a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")
		result := a.executeTool(context.Background(), "revise", map[string]any{"ref": "note#7", "content": "deploy the checkout-flow PR"})
		if brain.actionID != 7 || brain.actionText != "deploy the checkout-flow PR" {
			t.Errorf("expected SetActionText(7, ...), got id=%d text=%q", brain.actionID, brain.actionText)
		}
		if brain.updatedNoteID != 0 {
			t.Errorf("the rendered action line was overwritten through UpdateNote(%d, %q)", brain.updatedNoteID, brain.updatedNoteContent)
		}
		if result != "updated" {
			t.Errorf(`result = %q, want "updated"`, result)
		}
	})
}

// TestExecuteTool_Revise_Thread covers a "thread#N" ref: content corrects the thread's state (fix_thread's old job), and state/remove are refused since a thread has neither.
func TestExecuteTool_Revise_Thread(t *testing.T) {
	brain := &toolTestBrain{}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "revise", map[string]any{
		"ref": "thread#19", "content": "mf x mdev is a Google Meet call, unrelated to the ORA work",
	})
	if brain.updatedThreadID != 19 || brain.updatedThreadState != "mf x mdev is a Google Meet call, unrelated to the ORA work" {
		t.Errorf("thread %d updated to %q, want the correction against thread 19", brain.updatedThreadID, brain.updatedThreadState)
	}
	if result != "fixed" {
		t.Errorf(`result = %q, want "fixed"`, result)
	}

	if bad := a.executeTool(context.Background(), "revise", map[string]any{"ref": "thread#19", "state": "done"}); !strings.HasPrefix(bad, "error") {
		t.Errorf("expected state on a thread to be refused, got %q", bad)
	}
	if bad := a.executeTool(context.Background(), "revise", map[string]any{"ref": "thread#19", "remove": true}); !strings.HasPrefix(bad, "error") {
		t.Errorf("expected remove on a thread to be refused, got %q", bad)
	}
}

// TestExecuteTool_Revise_BadArgsAndStoreFailures verifies a malformed ref, an unhandled ref kind, a self-contradicting call, a call with nothing to change, and a failed store write are all reported as an "error: ..." result rather than a silent no-op or a false confirmation.
func TestExecuteTool_Revise_BadArgsAndStoreFailures(t *testing.T) {
	cases := []struct {
		name  string
		args  map[string]any
		brain *toolTestBrain
	}{
		{"no ref", map[string]any{"content": "something"}, &toolTestBrain{}},
		{"ref with no #", map[string]any{"ref": "note12", "content": "something"}, &toolTestBrain{}},
		{"ref of an unhandled kind", map[string]any{"ref": "episode#5", "content": "something"}, &toolTestBrain{}},
		{"remove with content", map[string]any{"ref": "note#5", "content": "x", "remove": true}, &toolTestBrain{}},
		{"nothing to change", map[string]any{"ref": "note#5"}, &toolTestBrain{}},
		{"UpdateNote fails", map[string]any{"ref": "note#105", "content": "x"}, &toolTestBrain{updateNoteErr: fmt.Errorf("db closed")}},
		{"DeleteNote fails", map[string]any{"ref": "note#105", "remove": true}, &toolTestBrain{deleteNoteErr: fmt.Errorf("db closed")}},
		{"SetActionStatus fails", map[string]any{"ref": "note#105", "state": "done"}, &toolTestBrain{actionErr: fmt.Errorf("no action item with id 105")}},
		{"UpdateThreadState fails", map[string]any{"ref": "thread#19", "content": "x"}, &toolTestBrain{updateThreadErr: fmt.Errorf("no thread with id 19")}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := NewAgent(nil, nil, tc.brain, nil, "FAKE_API_KEY")
			result := a.executeTool(context.Background(), "revise", tc.args)
			if !strings.HasPrefix(result, "error") {
				t.Errorf("expected an error result, got %q", result)
			}
		})
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

// TestExecuteTool_ReadFile_SensitivePath_BlocksOnApproval verifies a sensitive path blocks on ToolApprovalChan instead of shipping its content straight to the model, once an approver is registered (the terminal UI, which reads that channel — see SetToolApprovals).
func TestExecuteTool_ReadFile_SensitivePath_BlocksOnApproval(t *testing.T) {
	SetToolApprovals(true)
	t.Cleanup(func() { SetToolApprovals(false) })
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

// TestExecuteTool_ReadClipboard_BlocksOnApproval verifies read_clipboard always requires approval where an approver is registered, regardless of content — the clipboard can carry secrets a password manager just copied.
func TestExecuteTool_ReadClipboard_BlocksOnApproval(t *testing.T) {
	SetToolApprovals(true)
	t.Cleanup(func() { SetToolApprovals(false) })
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
		{"store failure", "revise", map[string]any{"ref": "note#7", "content": "the corrected fact"}, &toolTestBrain{updateNoteErr: fmt.Errorf("no note with id 7")}},
		{"missing ref", "revise", map[string]any{"remove": true}, &toolTestBrain{}},
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
func TestExecuteTool_QueryMemory_WindowGoesToTheStoreNotAPostFilter(t *testing.T) {
	brain := &toolTestBrain{windowedHybridHits: []db.MemoryHit{{Source: "episode", Content: "x", CreatedAt: time.Now()}}}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	a.executeTool(context.Background(), "query_memory", map[string]any{"query": "anything", "since": "2026-07-05", "until": "2026-07-06"})

	// A bare date means the whole day: since anchors to its midnight, until to its 23:59:59, both in local time.
	wantSince := time.Date(2026, 7, 5, 0, 0, 0, 0, time.Local)
	wantUntil := time.Date(2026, 7, 6, 23, 59, 59, 0, time.Local)
	if !brain.capturedSince.Equal(wantSince) {
		t.Errorf("since passed to the store = %v, want %v", brain.capturedSince, wantSince)
	}
	if !brain.capturedUntil.Equal(wantUntil) {
		t.Errorf("until passed to the store = %v, want %v", brain.capturedUntil, wantUntil)
	}
	// The store filters SQL-side now, so there is no over-fetch: a windowed call asks for the same limit as a plain one.
	if brain.capturedHybridLimit != queryMemoryHits {
		t.Errorf("windowed limit = %d, want %d (no over-fetch)", brain.capturedHybridLimit, queryMemoryHits)
	}

	a.executeTool(context.Background(), "query_memory", map[string]any{"query": "anything", "since": "2026-07-05"})
	if !brain.capturedUntil.IsZero() {
		t.Errorf("a missing until must stay open-ended (zero), got %v", brain.capturedUntil)
	}
}

// TestExecuteTool_QueryMemory_WindowEmptiedANonEmptyTopic_SaysSo verifies the honest-empty answer for a windowed query: when the topic exists but nothing falls inside the window, the model is told both facts instead of a bare "no memory matches" it would relay as "that never happened".
func TestExecuteTool_QueryMemory_WindowEmptiedANonEmptyTopic_SaysSo(t *testing.T) {
	now := time.Now()
	brain := &toolTestBrain{hybridHits: []db.MemoryHit{
		{Source: "episode", Content: "watching Suits", CreatedAt: now},
		{Source: "episode", Content: "watching more Suits", CreatedAt: now},
	}}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "query_memory", map[string]any{"query": "suits", "since": "2026-07-05", "until": "2026-07-05"})

	if strings.Contains(result, "watching") {
		t.Fatalf("expected no out-of-window content in the answer, got %q", result)
	}
	if !strings.Contains(result, "2 matches") || !strings.Contains(result, "none") {
		t.Errorf("expected the emptied-by-window case distinguished from a genuinely empty store, got %q", result)
	}
}

// TestExecuteTool_QueryMemory_RealStore_WindowConstrainsResults runs query_memory against a real db.Store: a windowed call must return only in-window content, and a window holding nothing must come back as the honest none-in-window answer, never the out-of-window hits.
func TestExecuteTool_QueryMemory_RealStore_WindowConstrainsResults(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if _, err := store.LogEpisode(ctx, "Code", "audit.md", "reviewing the retrieval audit"); err != nil {
		t.Fatalf("LogEpisode: %v", err)
	}
	a := NewAgent(nil, nil, store, nil, "FAKE_API_KEY")

	result := a.executeTool(ctx, "query_memory", map[string]any{"query": "retrieval audit", "since": "today"})
	if !strings.Contains(result, "reviewing the retrieval audit") {
		t.Errorf("expected today's episode inside a since-today window, got %q", result)
	}

	result = a.executeTool(ctx, "query_memory", map[string]any{"query": "retrieval audit", "until": "yesterday"})
	if strings.Contains(result, "reviewing") {
		t.Errorf("expected no content for a window before the episode existed, got %q", result)
	}
	if !strings.Contains(result, "1 matches") || !strings.Contains(result, "none until") {
		t.Errorf("expected the none-in-window answer naming what exists outside it, got %q", result)
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

// A window whose episodes overflow the cap is answered from the summary tier — every stretch of the window represented — never from the newest slice of raw episodes. A window that fits stays on episodes.
func TestExecuteTool_Recall_BigWindowClimbsToSummaries(t *testing.T) {
	brain := &toolTestBrain{}
	base := time.Date(2026, 8, 28, 9, 0, 0, 0, time.Local)
	for i := 0; i < recallEpisodeCap+10; i++ {
		brain.windowEpisodes = append(brain.windowEpisodes, db.Episode{
			CreatedAt: base.Add(time.Duration(i) * time.Minute), App: "Brave", Title: "evening stuff", ScreenText: "late night content",
		})
	}
	brain.windowSummaries = []db.WindowSummary{
		{CreatedAt: base.UTC(), Content: `{"task_name":"Climate Risk Statement Builder ASRS","summary":"scoring vulnerability data"}`},
		{CreatedAt: base.Add(8 * time.Hour).UTC(), Content: `{"task_name":"Ora Memory Architecture Development","summary":"recall surgery"}`},
		{CreatedAt: base.Add(9 * time.Hour).UTC(), Content: `{"task_name":"Raw Activity Log","summary":"Unknown | Unknown"}`},
	}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")
	out := a.ExecuteTool(context.Background(), "recall", map[string]any{"since": "2026-08-28", "until": "2026-08-28"})
	if !strings.Contains(out, "Climate Risk Statement Builder ASRS") || !strings.Contains(out, "Ora Memory Architecture Development") {
		t.Errorf("an overflowing window must answer from summaries covering the whole window, got:\n%s", out)
	}
	if strings.Contains(out, "late night content") {
		t.Errorf("summaries and raw episodes must not mix in one overflowing-window answer, got:\n%s", out)
	}
	if strings.Contains(out, "Raw Activity Log") {
		t.Errorf("the compiler's Unknown-window fallback bucket is noise and must be filtered, got:\n%s", out)
	}

	brain.windowEpisodes = brain.windowEpisodes[:5]
	out = a.ExecuteTool(context.Background(), "recall", map[string]any{"since": "2026-08-28", "until": "2026-08-28"})
	if !strings.Contains(out, "late night content") {
		t.Errorf("a window that fits must stay on raw episodes, got:\n%s", out)
	}
}

// "What do I owe?" is a question about a column, not about meaning, and query_memory answered it with ten summaries about attending meetings. The action_items tool reads the list directly, and leads each line with the id so revise can close one without a second lookup.
func TestExecuteTool_ActionItemsListsWhatIsOwed(t *testing.T) {
	brain := &toolTestBrain{openActions: []memory.ActionItem{
		{NoteID: 41, Owner: "Alex Rivera", Text: "push the value chain branch", Status: memory.StatusOpen, Priority: memory.PriorityNormal},
	}}
	got := NewAgent(nil, nil, brain, nil, "").ExecuteTool(context.Background(), "action_items", map[string]any{})
	if !strings.Contains(got, "[note#41]") || !strings.Contains(got, "push the value chain branch") {
		t.Errorf("got %q, want the item with its id", got)
	}
}

// Nothing outstanding is an answer, not an error: a model handed an empty result has to be told the list is empty rather than left to read silence as a failure.
func TestExecuteTool_ActionItemsSaysWhenNothingIsOwed(t *testing.T) {
	got := NewAgent(nil, nil, &toolTestBrain{}, nil, "").ExecuteTool(context.Background(), "action_items", map[string]any{})
	if !strings.Contains(got, "nothing outstanding") {
		t.Errorf("got %q", got)
	}
}

// "What were my meetings about yesterday" ranked screens of the user reading transcripts above the minutes themselves on 2026-09-02, because the summaries literally contain "meeting minutes" and the minutes do not. kind='meeting' skips the ranking and lists the minutes in the window, newest first.
func TestExecuteTool_QueryMemory_KindMeetingListsMinutes(t *testing.T) {
	day := time.Date(2026, 9, 1, 10, 0, 0, 0, time.Local)
	brain := &toolTestBrain{
		notes: []db.Note{
			{ID: 3, Kind: "meeting", Content: "# Standup — auth update", CreatedAt: day.Add(4 * time.Hour)},
			{ID: 2, Kind: "fact", Content: "user likes go", CreatedAt: day},
			{ID: 1, Kind: "meeting", Content: "# Old sync", CreatedAt: day.AddDate(0, 0, -3)},
		},
		hybridHits: []db.MemoryHit{{Source: "summary", Content: "reviewed meeting minutes in the terminal"}},
	}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "query_memory", map[string]any{"query": "meetings", "kind": "meeting", "since": "2026-09-01", "until": "2026-09-01"})

	if !strings.Contains(result, "[note#3]") || !strings.Contains(result, "Standup") {
		t.Errorf("expected the day's minutes, got %q", result)
	}
	if strings.Contains(result, "Old sync") || strings.Contains(result, "likes go") || strings.Contains(result, "reviewed meeting minutes") {
		t.Errorf("expected only meeting notes inside the window, got %q", result)
	}
}

// On 2026-09-02 every 3.1 Flash Live session closed with "You exceeded your current quota" a quarter second after connecting. Bisected with a probe: the prompt, the function tools, the voice, thinking and resumption all pass alone and together; adding Google Search grounding beside the function tools is what trips it. The 2.5 model takes both.
func TestLiveToolsFor_NoSearchOnLive3(t *testing.T) {
	for _, tool := range liveToolsFor("gemini-3.1-flash-live-preview") {
		if tool.GoogleSearch != nil {
			t.Fatal("a 3.x live model must not be handed Google Search grounding")
		}
	}
	found := false
	for _, tool := range liveToolsFor("gemini-2.5-flash-native-audio-preview-12-2025") {
		if tool.GoogleSearch != nil {
			found = true
		}
	}
	if !found {
		t.Fatal("the 2.5 live model keeps Google Search grounding")
	}
}

// An action item carries a priority as well as a status — the store holds [open/high] and [open/low] rows today — and one of the four tools revise replaced could set it. Dropping it left "make that one high priority" with nothing to call.
func TestExecuteTool_Revise_Priority(t *testing.T) {
	brain := &toolTestBrain{}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "revise", map[string]any{"ref": "note#7", "priority": "high"})

	if brain.actionID != 7 || brain.actionPriority != "high" {
		t.Errorf("expected SetActionPriority(7, high), got id=%d priority=%q", brain.actionID, brain.actionPriority)
	}
	if result != "updated" {
		t.Errorf("result = %q, want \"updated\"", result)
	}
	if bad := a.executeTool(context.Background(), "revise", map[string]any{"ref": "note#7", "priority": "urgent"}); !strings.HasPrefix(bad, "error") {
		t.Errorf("accepted an unknown priority: %q", bad)
	}
}

// screenFake stands in for the accessibility bus in the ring tests. `at` is where the element is right now, which is not where observe_screen listed it once the page has scrolled; `stale` is what the staleness check answers; `readErr` is what reading the rectangle answers when the element cannot be read at all.
type screenFake struct {
	at       act.Node
	stale    error
	readErr  error
	rings    []string
	clicked  []string
	verified []string
}

// ringingAgent wires an Agent that has one element to look at, can draw on the screen, and reads the element's rectangle through the fake rather than the bus. Input: the node observe_screen will list. Output: the agent and the fake, whose `at` a test moves to make the page scroll under the list.
func ringingAgent(t *testing.T, listed act.Node) (*Agent, *screenFake) {
	t.Helper()
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "FAKE_API_KEY")
	f := &screenFake{at: listed}
	a.observe = func(ctx context.Context) (string, string, []act.Node, error) {
		return "mail", "Inbox", []act.Node{listed}, nil
	}
	a.Point = func(x, y, w, h int, label string) {
		f.rings = append(f.rings, fmt.Sprintf("%s %d,%d %dx%d", label, x, y, w, h))
	}
	a.extents = func(ctx context.Context, ref string) (int, int, int, int, error) {
		if f.readErr != nil {
			return 0, 0, 0, 0, f.readErr
		}
		return f.at.X, f.at.Y, f.at.W, f.at.H, nil
	}
	a.verify = func(ctx context.Context, ref, role, label string, x, y, w, h int) error {
		f.verified = append(f.verified, fmt.Sprintf("%s %s %q %d,%d %dx%d", ref, role, label, x, y, w, h))
		return f.stale
	}
	a.doAction = func(ctx context.Context, ref string) (string, error) {
		f.clicked = append(f.clicked, ref)
		return "press", nil
	}
	return a, f
}

// The ring the user reads and the element the click presses have to be the same thing. observe_screen's list holds the rectangle each element occupied when the list was made, and the toolkit moves elements around under it, so the ring is drawn around the rectangle read back at the moment of drawing.
func TestExecuteTool_PointAt_RingsWhereTheElementIsNow(t *testing.T) {
	a, f := ringingAgent(t, act.Node{Role: "push button", Label: "Merge", X: 10, Y: 20, W: 80, H: 30, Showing: true, Ref: "r-merge"})
	a.executeTool(context.Background(), "observe_screen", map[string]any{})
	f.at.X, f.at.Y = 14, 24
	got := a.executeTool(context.Background(), "point_at", map[string]any{"n": float64(1), "label": "here"})
	if len(f.rings) != 1 || f.rings[0] != "here 14,24 80x30" {
		t.Errorf("rings = %v, want one ring where the element is now (14,24), not where the list left it (10,20); result %q", f.rings, got)
	}
}

// A page that scrolled between the list and the ring leaves the number pointing at the right element in the wrong place, and the element that has taken that place may be Delete. Nothing is drawn then: a ring the user reads before saying go must never be over something else.
func TestExecuteTool_PointAt_RefusesWhenTheElementHasMoved(t *testing.T) {
	a, f := ringingAgent(t, act.Node{Role: "push button", Label: "Merge", X: 10, Y: 20, W: 80, H: 30, Showing: true, Ref: "r-merge"})
	a.executeTool(context.Background(), "observe_screen", map[string]any{})
	f.stale = errors.New("it is now at 10,420 80x30, not 10,20 80x30")
	got := a.executeTool(context.Background(), "point_at", map[string]any{"n": float64(1), "label": "here"})
	if len(f.rings) != 0 {
		t.Errorf("rings = %v, want nothing drawn once the element has moved out from under its number", f.rings)
	}
	if !strings.Contains(got, "look again") {
		t.Errorf("result = %q, want it to tell the model to look again", got)
	}
	if len(f.verified) != 1 || !strings.Contains(f.verified[0], "10,20 80x30") {
		t.Errorf("the staleness check saw %v, want it given the rectangle observe_screen listed", f.verified)
	}
}

// An element that cannot be read has no rectangle to ring, and the one in the list is exactly the rectangle that may be over something else by now.
func TestExecuteTool_PointAt_RefusesWhenTheRectangleCannotBeRead(t *testing.T) {
	a, f := ringingAgent(t, act.Node{Role: "push button", Label: "Merge", X: 10, Y: 20, W: 80, H: 30, Showing: true, Ref: "r-merge"})
	a.executeTool(context.Background(), "observe_screen", map[string]any{})
	f.readErr = errors.New("the element no longer answers")
	got := a.executeTool(context.Background(), "point_at", map[string]any{"n": float64(1)})
	if len(f.rings) != 0 || !strings.Contains(got, "look again") {
		t.Errorf("rings = %v result = %q; want no ring and a note to look again", f.rings, got)
	}
}

// An element scrolled out of view answers with a rectangle of no size; a ring around it would be a mark in the corner of the screen.
func TestExecuteTool_PointAt_RefusesWhenTheElementHasNoSize(t *testing.T) {
	a, f := ringingAgent(t, act.Node{Role: "push button", Label: "Merge", X: 10, Y: 20, W: 80, H: 30, Showing: true, Ref: "r-merge"})
	a.executeTool(context.Background(), "observe_screen", map[string]any{})
	f.at.W, f.at.H = 0, 0
	got := a.executeTool(context.Background(), "point_at", map[string]any{"n": float64(1)})
	if len(f.rings) != 0 || !strings.Contains(got, "look again") {
		t.Errorf("rings = %v result = %q; want no ring and a note to look again", f.rings, got)
	}
}

// lookedAt takes one look and hands the picture over the way a brain's own round does, so a test that draws from picture coordinates is in the state a real turn is in by the time the model could have read anything off it. Output: the ask's own context, carrying the look state the look left behind — callers must reuse it for every later call that needs to see the same picture.
func lookedAt(t *testing.T, a *Agent) context.Context {
	t.Helper()
	ctx := withAskLookState(context.Background())
	a.executeTool(ctx, "look", map[string]any{})
	if _, ok := takeLook(ctx); !ok {
		t.Fatal("the look took no picture to hand over")
	}
	return ctx
}

// drawingAgent wires an Agent with two observed elements and a fake Draw that records every call, for the draw tool tests. Output: the agent and a pointer to the slice of calls, formatted as "shape points x,y,w,h label".
func drawingAgent(t *testing.T) (a *Agent, drawn *[]string) {
	t.Helper()
	a = NewAgent(nil, nil, &toolTestBrain{}, nil, "FAKE_API_KEY")
	a.observe = func(ctx context.Context) (string, string, []act.Node, error) {
		return "mail", "Inbox", []act.Node{
			{Role: "push button", Label: "Compose", X: 10, Y: 20, W: 80, H: 30, Showing: true, Ref: "r-1"},
			{Role: "push button", Label: "Send", X: 200, Y: 300, W: 40, H: 20, Showing: true, Ref: "r-2"},
		}, nil
	}
	drawn = &[]string{}
	a.Draw = func(shape string, points [][2]int, x, y, w, h int, label string) error {
		*drawn = append(*drawn, fmt.Sprintf("%s %v %d,%d,%d,%d %q", shape, points, x, y, w, h, label))
		return nil
	}
	// Raw coordinates are read off the last look, so a test that draws from them has to be able to take one. This camera hands back the whole screen at its own size, which maps every picture point to itself and leaves such a test reading the numbers it gave.
	a.capture = func(ctx context.Context) (tracker.Capture, error) {
		return tracker.Capture{Data: []byte("fake-jpeg-bytes"), Mime: "image/jpeg", W: 1920, H: 1080, Scale: 1}, nil
	}
	// A drawing that names an element checks the number still points at it and reads where it is now, so these two seams stand in for the accessibility bus; a test that wants an element to have moved replaces them through drawingScreen.
	a.verify = func(ctx context.Context, ref, role, label string, x, y, w, h int) error { return nil }
	a.extents = func(ctx context.Context, ref string) (int, int, int, int, error) {
		if ref == "r-1" {
			return 10, 20, 80, 30, nil
		}
		return 200, 300, 40, 20, nil
	}
	return a, drawn
}

// drawingScreen adds the two seams that say whether a listed element is still itself and where it is now, so a test can move an element under its number or make it unreadable. Input: an agent from drawingAgent. Output: the fake, whose moved rectangle is returned by the extents seam and whose stale error is returned by the verify seam.
func drawingScreen(a *Agent) *screenFake {
	f := &screenFake{}
	a.extents = func(ctx context.Context, ref string) (int, int, int, int, error) {
		if f.readErr != nil {
			return 0, 0, 0, 0, f.readErr
		}
		if f.at.W > 0 {
			return f.at.X, f.at.Y, f.at.W, f.at.H, nil
		}
		// An unmoved element reads back where the list put it, which is what the two observed nodes carry.
		if ref == "r-1" {
			return 10, 20, 80, 30, nil
		}
		return 200, 300, 40, 20, nil
	}
	a.verify = func(ctx context.Context, ref, role, label string, x, y, w, h int) error {
		f.verified = append(f.verified, fmt.Sprintf("%s %d,%d %dx%d", ref, x, y, w, h))
		return f.stale
	}
	return f
}

// A number that no longer points at the element observe_screen listed draws nothing, exactly as point_at refuses to ring one: the page has scrolled and something else is under that number now.
func TestExecuteTool_Draw_RefusesWhenAnElementHasMoved(t *testing.T) {
	for name, args := range map[string]map[string]any{
		"an arrow between two elements": {"shape": "arrow", "from": float64(1), "to": float64(2)},
		"a box around one element":      {"shape": "box", "on": float64(1)},
	} {
		t.Run(name, func(t *testing.T) {
			a, drawn := drawingAgent(t)
			f := drawingScreen(a)
			a.executeTool(context.Background(), "observe_screen", map[string]any{})
			f.stale = errors.New("it is now at 10,420 80x30, not 10,20 80x30")
			got := a.executeTool(context.Background(), "draw", args)
			if len(*drawn) != 0 {
				t.Errorf("drew %v, want nothing once the element has moved out from under its number", *drawn)
			}
			if !strings.Contains(got, "look again") {
				t.Errorf("result = %q, want it to tell the model to look again", got)
			}
		})
	}
}

// A drawing goes where the element is now rather than where the list remembered it, the same rule the ring follows, because the reference stays valid while the page scrolls under it.
func TestExecuteTool_Draw_UsesWhereTheElementIsNow(t *testing.T) {
	a, drawn := drawingAgent(t)
	f := drawingScreen(a)
	a.executeTool(context.Background(), "observe_screen", map[string]any{})
	f.at = act.Node{X: 10, Y: 420, W: 80, H: 30}
	a.executeTool(context.Background(), "draw", map[string]any{"shape": "box", "on": float64(1)})
	if len(*drawn) != 1 || !strings.Contains((*drawn)[0], "10,420,80,30") {
		t.Errorf("drew %v, want the box around the rectangle read back now", *drawn)
	}
}

// Points and rectangles the model supplies itself are its own coordinates, not references to anything observe_screen listed, so nothing is checked and they are drawn as given.
func TestExecuteTool_Draw_TakesTheModelsOwnCoordinatesAsGiven(t *testing.T) {
	a, drawn := drawingAgent(t)
	f := drawingScreen(a)
	a.executeTool(context.Background(), "observe_screen", map[string]any{})
	ctx := lookedAt(t, a)
	f.stale = errors.New("every element has moved")
	a.executeTool(ctx, "draw", map[string]any{"shape": "line", "points": []any{[]any{1.0, 2.0}, []any{3.0, 4.0}}})
	a.executeTool(ctx, "draw", map[string]any{"shape": "box", "rect": map[string]any{"x": 5.0, "y": 6.0, "w": 7.0, "h": 8.0}})
	if len(*drawn) != 2 {
		t.Fatalf("drew %v, want both shapes drawn from the coordinates given", *drawn)
	}
	if len(f.verified) != 0 {
		t.Errorf("checked %v, want no element checked when the model gave its own coordinates", f.verified)
	}
}

// from and to name elements by the numbers observe_screen just listed; draw has to turn those into the screen points the overlay actually draws through, which are each element's rectangle centre, not its top-left corner.
func TestExecuteTool_Draw_MapsElementNumbersToCentres(t *testing.T) {
	a, drawn := drawingAgent(t)
	a.executeTool(context.Background(), "observe_screen", map[string]any{})
	got := a.executeTool(context.Background(), "draw", map[string]any{"shape": "arrow", "from": float64(1), "to": float64(2), "label": "press send"})
	want := `arrow [[50 35] [220 310]] 0,0,0,0 "press send"`
	if len(*drawn) != 1 || (*drawn)[0] != want {
		t.Errorf("drawn = %v, want %q", *drawn, want)
	}
	if !strings.Contains(got, "arrow") {
		t.Errorf("result = %q, want it to name the shape drawn", got)
	}
}

// The model sometimes has raw screen coordinates already (an observe_screen list's centres, worked out itself) rather than two element numbers; draw has to take those through unchanged, with no observe_screen call required first.
func TestExecuteTool_Draw_FreePointsForm(t *testing.T) {
	a, drawn := drawingAgent(t)
	ctx := lookedAt(t, a)
	got := a.executeTool(ctx, "draw", map[string]any{
		"shape":  "line",
		"points": []any{[]any{float64(1), float64(2)}, []any{float64(3), float64(4)}},
		"label":  "path",
	})
	want := `line [[1 2] [3 4]] 0,0,0,0 "path"`
	if len(*drawn) != 1 || (*drawn)[0] != want {
		t.Errorf("drawn = %v, want %q", *drawn, want)
	}
	if !strings.Contains(got, "line") {
		t.Errorf("result = %q, want it to name the shape drawn", got)
	}
}

// path is the free-form multi-point shape: it needs at least three points and takes no from/to, unlike arrow and line.
func TestExecuteTool_Draw_PathNeedsThreePoints(t *testing.T) {
	a, drawn := drawingAgent(t)
	ctx := lookedAt(t, a)
	got := a.executeTool(ctx, "draw", map[string]any{
		"shape":  "path",
		"points": []any{[]any{float64(1), float64(1)}, []any{float64(2), float64(2)}},
	})
	if len(*drawn) != 0 {
		t.Errorf("drawn = %v, want nothing drawn for a path with only two points", *drawn)
	}
	if !strings.Contains(got, "at least 3") {
		t.Errorf("result = %q, want it to say path needs at least 3 points", got)
	}

	got = a.executeTool(ctx, "draw", map[string]any{
		"shape":  "path",
		"points": []any{[]any{float64(1), float64(1)}, []any{float64(2), float64(2)}, []any{float64(3), float64(3)}},
		"label":  "route",
	})
	want := `path [[1 1] [2 2] [3 3]] 0,0,0,0 "route"`
	if len(*drawn) != 1 || (*drawn)[0] != want {
		t.Errorf("drawn = %v, want %q", *drawn, want)
	}
	if !strings.Contains(got, "path") {
		t.Errorf("result = %q, want it to name the shape drawn", got)
	}
}

// box and circle draw around a rectangle rather than through points: "on" names a listed element, mapped to its own rectangle (not its centre, unlike arrow/line's from/to).
func TestExecuteTool_Draw_BoxAndCircleFromAnElementNumber(t *testing.T) {
	for _, shape := range []string{"box", "circle"} {
		t.Run(shape, func(t *testing.T) {
			a, drawn := drawingAgent(t)
			a.executeTool(context.Background(), "observe_screen", map[string]any{})
			got := a.executeTool(context.Background(), "draw", map[string]any{"shape": shape, "on": float64(1), "label": "here"})
			want := fmt.Sprintf(`%s [] 10,20,80,30 "here"`, shape)
			if len(*drawn) != 1 || (*drawn)[0] != want {
				t.Errorf("drawn = %v, want %q", *drawn, want)
			}
			if !strings.Contains(got, shape) {
				t.Errorf("result = %q, want it to name the shape drawn", got)
			}
		})
	}
}

// box and circle also take an explicit rect instead of an element number, for a region that was never one of observe_screen's elements.
func TestExecuteTool_Draw_BoxFromAnExplicitRect(t *testing.T) {
	a, drawn := drawingAgent(t)
	ctx := lookedAt(t, a)
	got := a.executeTool(ctx, "draw", map[string]any{
		"shape": "box",
		"rect":  map[string]any{"x": float64(1), "y": float64(2), "w": float64(3), "h": float64(4)},
	})
	want := `box [] 1,2,3,4 ""`
	if len(*drawn) != 1 || (*drawn)[0] != want {
		t.Errorf("drawn = %v, want %q", *drawn, want)
	}
	if !strings.Contains(got, "box") {
		t.Errorf("result = %q, want it to name the shape drawn", got)
	}
}

// box and circle need either on or rect; given neither, draw has to say so plainly rather than draw around a zeroed rectangle.
func TestExecuteTool_Draw_CircleRefusesWithNeitherOnNorRect(t *testing.T) {
	a, drawn := drawingAgent(t)
	got := a.executeTool(context.Background(), "draw", map[string]any{"shape": "circle"})
	if len(*drawn) != 0 {
		t.Errorf("drawn = %v, want nothing drawn with neither on nor rect", *drawn)
	}
	if !strings.Contains(got, "on") || !strings.Contains(got, "rect") {
		t.Errorf("result = %q, want it to name both on and rect as the ways to say where", got)
	}
}

func TestExecuteTool_Draw_RefusesABadShape(t *testing.T) {
	a, drawn := drawingAgent(t)
	a.executeTool(context.Background(), "observe_screen", map[string]any{})
	got := a.executeTool(context.Background(), "draw", map[string]any{"shape": "sparkle", "from": float64(1), "to": float64(2)})
	if len(*drawn) != 0 {
		t.Errorf("drawn = %v, want nothing drawn for a bad shape", *drawn)
	}
	if !strings.Contains(got, "arrow, line, path, box or circle") {
		t.Errorf("result = %q, want it to name the allowed shapes", got)
	}
}

func TestExecuteTool_Draw_RefusesAnUnknownElementNumber(t *testing.T) {
	a, drawn := drawingAgent(t)
	a.executeTool(context.Background(), "observe_screen", map[string]any{})
	got := a.executeTool(context.Background(), "draw", map[string]any{"shape": "arrow", "from": float64(1), "to": float64(9)})
	if len(*drawn) != 0 {
		t.Errorf("drawn = %v, want nothing drawn for an unknown element", *drawn)
	}
	if !strings.Contains(got, "no element 9") {
		t.Errorf("result = %q, want it to name the missing element", got)
	}
}

// click's staleness check has to be given the rectangle observe_screen listed, or it cannot tell an element that has moved from one that has not — and for an entry, which carries no label, the rectangle is the only thing that tells it from the next entry down the form.
func TestExecuteTool_Click_HandsTheStalenessCheckTheListedRectangle(t *testing.T) {
	a, f := ringingAgent(t, act.Node{Role: "entry", Label: "", X: 100, Y: 200, W: 300, H: 30, Showing: true, Ref: "r-entry"})
	a.executeTool(context.Background(), "observe_screen", map[string]any{})
	a.executeTool(context.Background(), "click", map[string]any{"n": float64(1)})
	if len(f.verified) != 1 || f.verified[0] != `r-entry entry "" 100,200 300x30` {
		t.Errorf("the staleness check saw %v, want the ref, role, label and rectangle from the list", f.verified)
	}
	if len(f.clicked) != 1 {
		t.Errorf("clicked = %v, want the click to go through when nothing has changed", f.clicked)
	}
}

// The confirmation ring is the whole of what the user is answering when they say go, so it is read back the same way point_at reads it.
func TestExecuteTool_Click_GuardedRingsWhereTheElementIsNow(t *testing.T) {
	a, f := ringingAgent(t, act.Node{Role: "push button", Label: "Send", X: 10, Y: 20, W: 80, H: 30, Showing: true, Ref: "r-send"})
	a.executeTool(context.Background(), "observe_screen", map[string]any{})
	f.at.X, f.at.Y = 12, 26
	got := a.executeTool(context.Background(), "click", map[string]any{"n": float64(1)})
	if len(f.clicked) != 0 {
		t.Errorf("clicked = %v, want a guarded element left alone without consent", f.clicked)
	}
	if len(f.rings) != 1 || f.rings[0] != "Send 12,26 80x30" {
		t.Errorf("rings = %v, want the confirmation ring where the element is now (12,26)", f.rings)
	}
	if !strings.HasPrefix(got, "Stopped before ") {
		t.Errorf("result = %q, want it to begin \"Stopped before \"", got)
	}
}

// The click result carries the window title read fresh right after the click, not the one observe_screen listed before it — that is exactly where a click that resumed or played the wrong thing shows up first (2026-09-05 trace: a "Resume, S16 E7" click played episode 7 when episode 6 was asked for).
func TestExecuteTool_Click_ResultCarriesTheWindowTitleAfterTheClick(t *testing.T) {
	a, _ := ringingAgent(t, act.Node{Role: "push button", Label: "Resume, S16 E7", X: 10, Y: 20, W: 80, H: 30, Showing: true, Ref: "r-resume"})
	a.executeTool(context.Background(), "observe_screen", map[string]any{})
	calls := 0
	a.observe = func(ctx context.Context) (string, string, []act.Node, error) {
		calls++
		if calls == 1 {
			// The front-window check right before the click runs still sees the window observe_screen listed from.
			return "mail", "Inbox", nil, nil
		}
		// The click itself navigated: the read right after it must carry the new window.
		return "Brave", "Watch Family Guy S16 Episode 7 on JioHotstar", nil, nil
	}
	got := a.executeTool(context.Background(), "click", map[string]any{"n": float64(1)})
	want := `clicked [1] push button "Resume, S16 E7" via press; the window is now "Watch Family Guy S16 Episode 7 on JioHotstar"; check it matches what was asked, then call observe_screen if you need the list`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A title read that fails after a successful click must not fail the click itself — the click already happened — so the result falls back to the plain form instead of losing the fact that the click went through.
func TestExecuteTool_Click_ResultFallsBackWhenTheTitleCannotBeRead(t *testing.T) {
	a, _ := ringingAgent(t, act.Node{Role: "push button", Label: "Merge", X: 10, Y: 20, W: 80, H: 30, Showing: true, Ref: "r-merge"})
	a.executeTool(context.Background(), "observe_screen", map[string]any{})
	a.observe = func(ctx context.Context) (string, string, []act.Node, error) {
		return "", "", nil, errors.New("the bus is gone")
	}
	got := a.executeTool(context.Background(), "click", map[string]any{"n": float64(1)})
	want := `clicked [1] push button "Merge" via press; call observe_screen to see the result`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// When the element cannot be read there is nothing to show the user, so the guarded click is refused with a note to look again rather than with a ring drawn from the list — and it is still not clicked.
func TestExecuteTool_Click_GuardedWithoutAReadableRectangle(t *testing.T) {
	a, f := ringingAgent(t, act.Node{Role: "push button", Label: "Send", X: 10, Y: 20, W: 80, H: 30, Showing: true, Ref: "r-send"})
	a.executeTool(context.Background(), "observe_screen", map[string]any{})
	f.readErr = errors.New("the element no longer answers")
	got := a.executeTool(context.Background(), "click", map[string]any{"n": float64(1)})
	if len(f.rings) != 0 || len(f.clicked) != 0 {
		t.Errorf("rings = %v clicked = %v; want neither a ring nor a click", f.rings, f.clicked)
	}
	if !strings.Contains(got, "look again") {
		t.Errorf("result = %q, want it to tell the model to look again", got)
	}
}

// lookingAgent is drawingAgent with a fake camera: a look returns a fixed picture of the screen from 0,32 down, two screen pixels to one picture pixel, so a test can check both the mapping and what the model is told. Output: the agent and a pointer to the count of looks the fake camera has taken.
func lookingAgent(t *testing.T) (a *Agent, drawn *[]string, taken *int) {
	t.Helper()
	a, drawn = drawingAgent(t)
	n := 0
	a.capture = func(ctx context.Context) (tracker.Capture, error) {
		n++
		return tracker.Capture{Data: []byte("fake-jpeg-bytes"), Mime: "image/jpeg", X: 0, Y: 32, W: 1280, H: 704, Scale: 2}, nil
	}
	return a, drawn, &n
}

// A look has to tell the model the three things that make a point it reads off the picture usable: how big the picture is, where its top-left corner sits on the screen, and how many screen pixels one of its own pixels is worth.
func TestExecuteTool_Look_SaysHowBigThePictureIsAndWhereItCameFrom(t *testing.T) {
	a, _, taken := lookingAgent(t)
	ctx := withAskLookState(context.Background())
	got := a.executeTool(ctx, "look", map[string]any{})
	if *taken != 1 {
		t.Fatalf("the camera ran %d times, want once", *taken)
	}
	for _, want := range []string{"1280", "704", "0,32"} {
		if !strings.Contains(got, want) {
			t.Errorf("result = %q, want it to carry %q", got, want)
		}
	}
	img, ok := takeLook(ctx)
	if !ok || string(img.Data) != "fake-jpeg-bytes" {
		t.Errorf("takeLook = %v %v, want the picture waiting to be handed to the model", img, ok)
	}
	if _, again := takeLook(ctx); again {
		t.Error("the same picture was handed over twice; it is sent once, with the tool result that produced it")
	}
}

// Pictures are the most expensive thing an ask can send, so a turn may take two and no more.
func TestExecuteTool_Look_StopsAtTwoPerAsk(t *testing.T) {
	a, _, taken := lookingAgent(t)
	ctx := withAskLookState(context.Background())
	a.executeTool(ctx, "look", map[string]any{})
	a.executeTool(ctx, "look", map[string]any{})
	got := a.executeTool(ctx, "look", map[string]any{})
	if *taken != maxLooksPerAsk {
		t.Errorf("the camera ran %d times, want the cap of %d", *taken, maxLooksPerAsk)
	}
	if !strings.HasPrefix(got, "error") {
		t.Errorf("the third look answered %q, want a refusal", got)
	}
	ctx = withAskLookState(context.Background())
	if got := a.executeTool(ctx, "look", map[string]any{}); strings.HasPrefix(got, "error") {
		t.Errorf("the next ask's first look answered %q, want the cap to have been reset with the ask", got)
	}
}

// What a look costs is recorded, because no provider breaks its input count down by part and an ask that sent two screenfuls would otherwise look exactly like one that sent none.
func TestExecuteTool_Look_RecordsWhatThePictureCost(t *testing.T) {
	a, _, _ := lookingAgent(t)
	ctx := withAskLookState(context.Background())
	a.executeTool(ctx, "look", map[string]any{})
	if got, want := lookTokensSpent(ctx), lookTokenCost(1280, 704); got != want || got == 0 {
		t.Errorf("look tokens = %d, want %d", got, want)
	}
}

// A routine and a typed question can both be mid-ask on the same Agent at once; each must get back only the picture its own look took. The look state used to live on the shared Agent, so one ask's look could clobber the other's before either read it back — this drives two looks through one Agent at the same time and checks each ask's own context still holds only its own picture.
func TestExecuteTool_Look_TwoConcurrentAsksDoNotShareAPicture(t *testing.T) {
	type askIDKey struct{}
	a, _ := drawingAgent(t)
	a.capture = func(ctx context.Context) (tracker.Capture, error) {
		id, _ := ctx.Value(askIDKey{}).(string)
		return tracker.Capture{Data: []byte(id), Mime: "image/jpeg", W: 100, H: 100, Scale: 1}, nil
	}

	ctxA := withAskLookState(context.WithValue(context.Background(), askIDKey{}, "routine's picture"))
	ctxB := withAskLookState(context.WithValue(context.Background(), askIDKey{}, "typed ask's picture"))

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a.executeTool(ctxA, "look", map[string]any{}) }()
	go func() { defer wg.Done(); a.executeTool(ctxB, "look", map[string]any{}) }()
	wg.Wait()

	imgA, okA := takeLook(ctxA)
	imgB, okB := takeLook(ctxB)
	if !okA || string(imgA.Data) != "routine's picture" {
		t.Errorf("the routine's look = %v %v, want its own picture back", imgA, okA)
	}
	if !okB || string(imgB.Data) != "typed ask's picture" {
		t.Errorf("the typed ask's look = %v %v, want its own picture back", imgB, okB)
	}
}

// This is the whole bug of the 2026-09-05 run: inside a video there are no accessibility items, so the model guessed two coordinates and drew both arrows in the wrong place. Raw coordinates are only meaningful against a picture the model has actually seen, so without one nothing is drawn.
func TestExecuteTool_Draw_RefusesRawCoordinatesWithoutALook(t *testing.T) {
	for name, args := range map[string]map[string]any{
		"free points":           {"shape": "arrow", "points": []any{[]any{10.0, 20.0}, []any{30.0, 40.0}}},
		"an explicit rectangle": {"shape": "box", "rect": map[string]any{"x": 10.0, "y": 20.0, "w": 30.0, "h": 40.0}},
	} {
		t.Run(name, func(t *testing.T) {
			a, drawn, _ := lookingAgent(t)
			got := a.executeTool(context.Background(), "draw", args)
			if len(*drawn) != 0 {
				t.Errorf("drew %v, want nothing drawn from coordinates read off no picture", *drawn)
			}
			if !strings.Contains(got, "I need to look at the screen first") {
				t.Errorf("result = %q, want it to say a look has to come first", got)
			}
		})
	}
}

// A point the model reads off the picture is in the picture's own pixels; the screen is somewhere else entirely, and draw is what maps one to the other.
func TestExecuteTool_Draw_MapsPicturePointsToTheScreen(t *testing.T) {
	a, drawn, _ := lookingAgent(t)
	ctx := lookedAt(t, a)
	a.executeTool(ctx, "draw", map[string]any{"shape": "arrow", "points": []any{[]any{100.0, 100.0}, []any{200.0, 300.0}}, "label": "Stewie"})
	want := `arrow [[200 232] [400 632]] 0,0,0,0 "Stewie"`
	if len(*drawn) != 1 || (*drawn)[0] != want {
		t.Errorf("drawn = %v, want %q: 2 screen pixels to a picture pixel, from 0,32 down", *drawn, want)
	}
	*drawn = nil
	a.executeTool(ctx, "draw", map[string]any{"shape": "box", "rect": map[string]any{"x": 100.0, "y": 100.0, "w": 50.0, "h": 25.0}})
	if len(*drawn) != 1 || !strings.Contains((*drawn)[0], "200,232,100,50") {
		t.Errorf("drawn = %v, want the rectangle scaled and moved onto the screen too", *drawn)
	}
}

// A coordinate outside the picture was not read off it, which is the other half of the same guess.
func TestExecuteTool_Draw_RefusesAPointOutsideThePicture(t *testing.T) {
	a, drawn, _ := lookingAgent(t)
	ctx := lookedAt(t, a)
	got := a.executeTool(ctx, "draw", map[string]any{"shape": "line", "points": []any{[]any{10.0, 10.0}, []any{1900.0, 300.0}}})
	if len(*drawn) != 0 {
		t.Errorf("drew %v, want nothing drawn for a point off the edge of the picture", *drawn)
	}
	if !strings.Contains(got, "1280") {
		t.Errorf("result = %q, want it to say how big the picture actually is", got)
	}
}

// Element numbers are unaffected: an element observe_screen listed carries its own screen rectangle, and no look is needed to draw around it.
func TestExecuteTool_Draw_ElementNumbersNeedNoLook(t *testing.T) {
	a, drawn, _ := lookingAgent(t)
	a.executeTool(context.Background(), "observe_screen", map[string]any{})
	a.executeTool(context.Background(), "draw", map[string]any{"shape": "box", "on": float64(1)})
	if len(*drawn) != 1 {
		t.Errorf("drawn = %v, want the box around the listed element with no look taken", *drawn)
	}
}

// fakeInput is a keyboard and pointer that records what was sent instead of touching the user's own screen, standing in for the portal session the daemon opens.
type fakeInput struct {
	opens int
	calls []string
	err   error
}

func (f *fakeInput) PressKey(name string) error {
	f.calls = append(f.calls, "press "+name)
	return f.err
}

func (f *fakeInput) TypeText(text string) error {
	f.calls = append(f.calls, "type "+text)
	return f.err
}

func (f *fakeInput) ClickAt(x, y float64) error {
	f.calls = append(f.calls, fmt.Sprintf("click %.0f,%.0f", x, y))
	return f.err
}

func (f *fakeInput) ScrollAt(x, y float64, dy int32) error {
	f.calls = append(f.calls, fmt.Sprintf("scroll %.0f,%.0f %d", x, y, dy))
	return f.err
}

// typingAgent is a lookingAgent whose keyboard, pointer and accessibility typing are all fakes, for the press_key, click_at, scroll_at and type_text tests. Output: the agent, the recorded keyboard and pointer, and the text the accessibility path was asked to set (its error, when set, is what a field with no text-setting action looks like).
func typingAgent(t *testing.T) (a *Agent, in *fakeInput) {
	t.Helper()
	a, _, _ = lookingAgent(t)
	in = &fakeInput{}
	a.input = onceInput(func(ctx context.Context) (InputDevice, error) {
		in.opens++
		return in, nil
	})
	return a, in
}

// A key press is the only way to reach Enter, Escape, Tab or a chord: nothing in the accessibility list offers them.
func TestExecuteTool_PressKey_PressesTheKey(t *testing.T) {
	a, in := typingAgent(t)
	got := a.executeTool(context.Background(), "press_key", map[string]any{"keys": "Ctrl+L"})
	if len(in.calls) != 1 || in.calls[0] != "press Ctrl+L" {
		t.Errorf("keyboard = %v, want the chord pressed once", in.calls)
	}
	if !strings.Contains(got, "Ctrl+L") {
		t.Errorf("result = %q, want it to name the key it pressed", got)
	}
}

// A session with no keyboard says so rather than failing silently, the same way look says this session cannot see the screen.
func TestExecuteTool_PressKey_SaysWhenThereIsNoKeyboard(t *testing.T) {
	a, _, _ := lookingAgent(t)
	got := a.executeTool(context.Background(), "press_key", map[string]any{"keys": "Enter"})
	if !strings.HasPrefix(got, "error") {
		t.Errorf("result = %q, want an error saying this session cannot reach the keyboard", got)
	}
}

// Enter on a focused Send button sends the message as surely as clicking it, so it stops at the same line.
func TestExecuteTool_PressKey_StopsBeforeEnterOnASendButton(t *testing.T) {
	a, in := typingAgent(t)
	a.executeTool(context.Background(), "observe_screen", map[string]any{})
	a.rememberClick(context.Background(), act.Item{N: 2, Role: "push button", Label: "Send", Ref: "r-2"})
	got := a.executeTool(context.Background(), "press_key", map[string]any{"keys": "Enter"})
	if len(in.calls) != 0 {
		t.Errorf("keyboard = %v, want nothing pressed", in.calls)
	}
	if !strings.HasPrefix(got, "Stopped before ") || !strings.Contains(got, "yes, send it") {
		t.Errorf("result = %q, want a stop naming what to say to unlock it", got)
	}
}

// The user's own go-ahead for this exact step unlocks the same press, the way it unlocks the click.
func TestExecuteTool_PressKey_EnterGoesThroughWhenTheUserSaidGo(t *testing.T) {
	a, in := typingAgent(t)
	a.executeTool(context.Background(), "observe_screen", map[string]any{})
	a.rememberClick(context.Background(), act.Item{N: 2, Role: "push button", Label: "Send", Ref: "r-2"})
	a.executeTool(WithGo(context.Background()), "press_key", map[string]any{"keys": "Enter"})
	if len(in.calls) != 1 || in.calls[0] != "press Enter" {
		t.Errorf("keyboard = %v, want the key pressed once the user said go", in.calls)
	}
}

// Only the keys that press what has focus are gated: Tab moves on and commits to nothing, whatever button is focused.
func TestExecuteTool_PressKey_TabOnASendButtonIsNotStopped(t *testing.T) {
	a, in := typingAgent(t)
	a.executeTool(context.Background(), "observe_screen", map[string]any{})
	a.rememberClick(context.Background(), act.Item{N: 2, Role: "push button", Label: "Send", Ref: "r-2"})
	a.executeTool(context.Background(), "press_key", map[string]any{"keys": "Tab"})
	if len(in.calls) != 1 || in.calls[0] != "press Tab" {
		t.Errorf("keyboard = %v, want Tab pressed", in.calls)
	}
}

// A point read off the last look names a place on the screen; the click has to land there, not at the picture's own numbers.
func TestExecuteTool_ClickAt_MapsThePictureCoordinatesOntoTheScreen(t *testing.T) {
	a, in := typingAgent(t)
	ctx := lookedAt(t, a)
	got := a.executeTool(ctx, "click_at", map[string]any{"x": 10.0, "y": 20.0})
	if len(in.calls) != 1 || in.calls[0] != "click 20,72" {
		t.Errorf("pointer = %v, want the picture point mapped onto the screen", in.calls)
	}
	if !strings.Contains(got, "20,72") {
		t.Errorf("result = %q, want it to say where it clicked", got)
	}
}

// A coordinate with no picture behind it is a guess, and a guessed click lands on whatever happens to be there.
func TestExecuteTool_ClickAt_RefusesWithoutALook(t *testing.T) {
	a, in := typingAgent(t)
	got := a.executeTool(withAskLookState(context.Background()), "click_at", map[string]any{"x": 10.0, "y": 20.0})
	if len(in.calls) != 0 {
		t.Errorf("pointer = %v, want nothing clicked", in.calls)
	}
	if !strings.Contains(got, "look at the screen first") {
		t.Errorf("result = %q, want the refusal that names the look to take", got)
	}
}

// A coordinate carries no label, so the window's own title is the only thing the stop line can read — and a checkout page names the commitment even when the button under the pointer does not.
func TestExecuteTool_ClickAt_StopsWhenTheWindowNamesAnIrreversibleAction(t *testing.T) {
	a, in := typingAgent(t)
	a.observe = func(ctx context.Context) (string, string, []act.Node, error) {
		return "shop", "Checkout", nil, nil
	}
	ctx := lookedAt(t, a)
	got := a.executeTool(ctx, "click_at", map[string]any{"x": 10.0, "y": 20.0})
	if len(in.calls) != 0 {
		t.Errorf("pointer = %v, want nothing clicked", in.calls)
	}
	if !strings.HasPrefix(got, "Stopped before ") || !strings.Contains(got, "no label") || !strings.Contains(got, "yes, checkout") {
		t.Errorf("result = %q, want a stop saying the window title is all it has and what to say to unlock it", got)
	}
}

// Scrolling at a point is for the panes and players that have no element to scroll to.
func TestExecuteTool_ScrollAt_ScrollsAtThePictureCoordinates(t *testing.T) {
	a, in := typingAgent(t)
	ctx := lookedAt(t, a)
	a.executeTool(ctx, "scroll_at", map[string]any{"x": 10.0, "y": 20.0, "dy": 3.0})
	if len(in.calls) != 1 || in.calls[0] != "scroll 20,72 3" {
		t.Errorf("pointer = %v, want three steps at the mapped point", in.calls)
	}
}

// The portal asks the user to allow remote control when its session opens, so it opens on the first call that needs it and never again.
func TestExecuteTool_Input_OpensOnceAcrossCalls(t *testing.T) {
	a, in := typingAgent(t)
	ctx := lookedAt(t, a)
	if in.opens != 0 {
		t.Fatalf("opened %d times before any call needed the keyboard, want none", in.opens)
	}
	a.executeTool(ctx, "press_key", map[string]any{"keys": "Escape"})
	a.executeTool(ctx, "click_at", map[string]any{"x": 10.0, "y": 20.0})
	a.executeTool(ctx, "scroll_at", map[string]any{"x": 10.0, "y": 20.0, "dy": 1.0})
	if in.opens != 1 {
		t.Errorf("opened %d times, want once for the whole session", in.opens)
	}
}

// type_text is the keyboard: the text, with its Enter when asked for, goes through the one input session press_key uses, so a user who types and then presses a key is asked for consent once.
func TestExecuteTool_TypeText_GoesThroughTheKeyboard(t *testing.T) {
	a, in := typingAgent(t)
	got := a.executeTool(context.Background(), "type_text", map[string]any{"text": "hello", "enter": true})
	if len(in.calls) != 1 || in.calls[0] != "type hello\n" {
		t.Errorf("keyboard = %v, want the text and its Enter typed once", in.calls)
	}
	if strings.HasPrefix(got, "error") || !strings.Contains(got, "typed 6 characters") {
		t.Errorf("result = %q, want the count of what was typed", got)
	}
}

// A keyboard that refuses, which is what a declined consent dialog looks like, is reported as a failure to type rather than as text that went in.
func TestExecuteTool_TypeText_ReportsAKeyboardThatRefused(t *testing.T) {
	a, in := typingAgent(t)
	in.err = errors.New("the portal session is gone")
	got := a.executeTool(context.Background(), "type_text", map[string]any{"text": "hello"})
	if !strings.HasPrefix(got, "error") || !strings.Contains(got, "could not type") {
		t.Errorf("result = %q, want an error naming the refusal", got)
	}
}

// A click at a point leaves this session unable to say what the keyboard is pointing at: the point carries no element, so the field the text would go into cannot be named and the text is refused rather than typed blind. A fresh observe_screen puts the model back to naming elements, and the typing goes through again.
func TestExecuteTool_TypeText_RefusesWhileTheFocusIsUnknownAfterAClickAt(t *testing.T) {
	a, in := typingAgent(t)
	ctx := lookedAt(t, a)
	a.executeTool(ctx, "observe_screen", map[string]any{})
	a.executeTool(ctx, "click_at", map[string]any{"x": 400.0, "y": 400.0})
	got := a.executeTool(ctx, "type_text", map[string]any{"text": "hello"})
	if len(in.calls) != 1 {
		t.Errorf("keyboard = %v, want only the click and nothing typed", in.calls)
	}
	if !strings.HasPrefix(got, "Stopped before ") || !strings.Contains(got, "could not identify") {
		t.Errorf("result = %q, want a stop saying the field could not be identified", got)
	}
	if got := a.executeTool(WithGo(ctx), "type_text", map[string]any{"text": "hello"}); strings.HasPrefix(got, "Stopped before ") {
		t.Errorf("result = %q, want the user's own go-ahead to unlock the typing", got)
	}
	a.executeTool(ctx, "observe_screen", map[string]any{})
	if got := a.executeTool(ctx, "type_text", map[string]any{"text": "hello"}); strings.HasPrefix(got, "Stopped before ") {
		t.Errorf("result = %q, want a fresh observe_screen to restore a known focus", got)
	}
}

// Enter presses whatever has focus, so it is refused for the same reason as the typing while this session cannot say what that is.
func TestExecuteTool_PressKey_RefusesEnterWhileTheFocusIsUnknown(t *testing.T) {
	a, in := typingAgent(t)
	ctx := lookedAt(t, a)
	a.executeTool(ctx, "observe_screen", map[string]any{})
	a.executeTool(ctx, "click_at", map[string]any{"x": 400.0, "y": 400.0})
	got := a.executeTool(ctx, "press_key", map[string]any{"keys": "Enter"})
	if len(in.calls) != 1 {
		t.Errorf("keyboard = %v, want only the click and no key pressed", in.calls)
	}
	if !strings.HasPrefix(got, "Stopped before ") || !strings.Contains(got, "could not identify") {
		t.Errorf("result = %q, want a stop saying the focused control could not be identified", got)
	}
}

// Tab moves the keyboard off whatever the last click focused, so the Enter after it is checked against nothing this session can vouch for and is refused rather than pressed on a control it cannot name.
func TestExecuteTool_PressKey_TabLeavesTheFocusUnknownForTheNextEnter(t *testing.T) {
	a, in := typingAgent(t)
	ctx := context.Background()
	a.executeTool(ctx, "observe_screen", map[string]any{})
	a.rememberClick(ctx, act.Item{N: 1, Role: "push button", Label: "Compose", Ref: "r-1"})
	a.executeTool(ctx, "press_key", map[string]any{"keys": "Tab"})
	got := a.executeTool(ctx, "press_key", map[string]any{"keys": "Enter"})
	if len(in.calls) != 1 || in.calls[0] != "press Tab" {
		t.Errorf("keyboard = %v, want the Tab pressed and the Enter refused", in.calls)
	}
	if !strings.HasPrefix(got, "Stopped before ") || !strings.Contains(got, "could not identify") {
		t.Errorf("result = %q, want a stop saying the focused control could not be identified", got)
	}
}

// A point on the screen does have a name when the accessibility list covers it: the item whose rectangle holds the point goes through the same stop line a numbered click does, and the result says what the point landed on.
func TestExecuteTool_ClickAt_ChecksTheItemUnderThePoint(t *testing.T) {
	a, in := typingAgent(t)
	ctx := lookedAt(t, a)
	a.executeTool(ctx, "observe_screen", map[string]any{})
	// The picture's top-left is 0,32 and one of its pixels is two screen pixels, so 100,134 in it is 200,300 on the screen — the Send button's own corner.
	got := a.executeTool(ctx, "click_at", map[string]any{"x": 100.0, "y": 134.0})
	if len(in.calls) != 0 {
		t.Errorf("pointer = %v, want nothing clicked", in.calls)
	}
	if !strings.HasPrefix(got, "Stopped before ") || !strings.Contains(got, "Send") || !strings.Contains(got, "yes, send it") {
		t.Errorf("result = %q, want a stop naming the item the point lands on", got)
	}
	got = a.executeTool(ctx, "click_at", map[string]any{"x": 25.0, "y": 4.0})
	if len(in.calls) != 1 || in.calls[0] != "click 50,40" {
		t.Errorf("pointer = %v, want the harmless point clicked", in.calls)
	}
	if !strings.Contains(got, "Compose") {
		t.Errorf("result = %q, want it to say which item the point landed on", got)
	}
}

// A newline inside the text is a real Enter keystroke, which submits a chat box halfway through the message, and a tab moves the rest of the text into another field. Neither is what the model asked for, so control characters are refused and press_key is named as the way to send a key on purpose.
func TestExecuteTool_TypeText_RefusesControlCharactersInsideTheText(t *testing.T) {
	a, in := typingAgent(t)
	got := a.executeTool(context.Background(), "type_text", map[string]any{"text": "line one\nline two"})
	if len(in.calls) != 0 {
		t.Errorf("keyboard = %v, want nothing typed", in.calls)
	}
	if !strings.Contains(got, "press_key") {
		t.Errorf("result = %q, want a refusal naming press_key for keys", got)
	}
}

// Two asks answered at once by the same agent must not read each other's stop-line state. This is the interleaving they produce: the routine clicks Send, the typed ask clicks Compose, and each then presses Enter — the routine's must stop and the typed ask's must go through.
func TestExecuteTool_StopLineStateBelongsToOneAsk(t *testing.T) {
	a, in := typingAgent(t)
	routine := withAskLookState(context.Background())
	typed := withAskLookState(context.Background())
	a.executeTool(routine, "observe_screen", map[string]any{})
	a.rememberClick(routine, act.Item{N: 2, Role: "push button", Label: "Send", Ref: "r-2"})
	a.executeTool(typed, "observe_screen", map[string]any{})
	a.rememberClick(typed, act.Item{N: 1, Role: "push button", Label: "Compose", Ref: "r-1"})
	if got := a.executeTool(routine, "press_key", map[string]any{"keys": "Enter"}); !strings.HasPrefix(got, "Stopped before ") || !strings.Contains(got, "Send") {
		t.Errorf("the routine's Enter = %q, want it stopped against its own Send button", got)
	}
	if got := a.executeTool(typed, "press_key", map[string]any{"keys": "Enter"}); strings.HasPrefix(got, "Stopped before ") {
		t.Errorf("the typed ask's Enter = %q, want it through against its own harmless button", got)
	}
	if len(in.calls) != 1 || in.calls[0] != "press Enter" {
		t.Errorf("keyboard = %v, want only the typed ask's Enter", in.calls)
	}
}

// The portal's consent dialog is the first thing an open waits on, and an ask that times out waiting for it must not cost the session its keyboard forever: only a successful open is remembered, so the next call asks again.
func TestOnceInput_RetriesAfterAFailedOpen(t *testing.T) {
	opens := 0
	in := &fakeInput{}
	open := onceInput(func(ctx context.Context) (InputDevice, error) {
		opens++
		if opens == 1 {
			return nil, errors.New("the consent dialog timed out")
		}
		return in, nil
	})
	if _, err := open(context.Background()); err == nil {
		t.Fatal("the first open must report the failure")
	}
	dev, err := open(context.Background())
	if err != nil || dev == nil {
		t.Fatalf("second open = %v, %v, want the failed open retried", dev, err)
	}
	if _, err := open(context.Background()); err != nil || opens != 2 {
		t.Errorf("opened %d times, err %v, want the successful open remembered", opens, err)
	}
}

// Every new tool has to be offered to a screen round, allowed through the ask gate, and kept out of the log file, or it is declared and unusable.
func TestScreenToolLists_CarryTheKeyboardAndPointerTools(t *testing.T) {
	for _, name := range []string{"press_key", "click_at", "scroll_at"} {
		if !askAllowedTools[name] || !screenRoundTools[name] || !screenToolNames[name] {
			t.Errorf("%s: allowed=%v screenRound=%v screenNames=%v, want all three", name, askAllowedTools[name], screenRoundTools[name], screenToolNames[name])
		}
	}
}

// shellOverviewOpen replays the keys pressed so far the way GNOME's own overview answers them, so a test's observe can show the shell's search box exactly while it would really be on the user's screen: Super opens it and, pressed again, closes it; Escape closes it; Enter closes it only when something was actually activated, since a search that matched nothing leaves the overview up. Input: the keys recorded so far, and whether the front window has changed since the switch began. Output: whether the overview is showing right now.
func shellOverviewOpen(calls []string, frontChanged bool) bool {
	open := false
	for _, c := range calls {
		switch c {
		case "press Super":
			open = !open
		case "press Escape":
			open = false
		case "press Enter":
			open = !frontChanged
		}
	}
	return open
}

// overviewNodes is what the shell publishes over the accessibility bus while its overview search is up: the box the name is typed into, showing on screen.
func overviewNodes() []act.Node {
	return []act.Node{{Role: "text", Label: "Type to search", Showing: true}}
}

// switchingAgent is a typingAgent whose window in front is whatever front() says at the moment it is asked, so a test can have another application come forward the moment the keys land. While the keys pressed so far mean GNOME's overview would be up (see shellOverviewOpen) it reads as the shell and its search box instead, which is what switch_window checks before it types. The waits between the keys and the verification budget are zeroed for the run of the test, so a switch costs no wall time here. Input: the test and a function returning the app and title in front now. Output: the agent and the keyboard that records what was pressed.
func switchingAgent(t *testing.T, front func() (string, string)) (*Agent, *fakeInput) {
	t.Helper()
	a, in := typingAgent(t)
	startApp, startTitle := front()
	a.observe = func(ctx context.Context) (string, string, []act.Node, error) {
		app, title := front()
		if shellOverviewOpen(in.calls, app != startApp || title != startTitle) {
			return "gnome-shell", "Activities", overviewNodes(), nil
		}
		return app, title, nil, nil
	}
	keyWait, verifyFor := switchKeyWait, switchVerifyFor
	switchKeyWait, switchVerifyFor = 0, 0
	t.Cleanup(func() { switchKeyWait, switchVerifyFor = keyWait, verifyFor })
	return a, in
}

// A job stays in the window it started in: leaving it needs the user's own request to name the other application, so a model that decides on its own to go somewhere else is refused before a key is pressed.
func TestExecuteTool_SwitchWindow_RefusesWhenTheRequestDoesNotNameTheApp(t *testing.T) {
	a, in := switchingAgent(t, func() (string, string) { return "mail", "Inbox" })
	ctx := WithQuestion(context.Background(), "read me the top line")
	got := a.executeTool(ctx, "switch_window", map[string]any{"app": "Brave"})
	if len(in.calls) != 0 {
		t.Errorf("keyboard = %v, want nothing pressed", in.calls)
	}
	if !strings.HasPrefix(got, "error") || !strings.Contains(got, "Brave") {
		t.Errorf("result = %q, want a refusal naming the app the request never asked for", got)
	}
}

// Switching to the window that is already in front presses keys over the user's screen for nothing.
func TestExecuteTool_SwitchWindow_SaysWhenTheAppIsAlreadyInFront(t *testing.T) {
	a, in := switchingAgent(t, func() (string, string) { return "Brave", "News" })
	ctx := WithQuestion(context.Background(), "switch to Brave and read the headline")
	got := a.executeTool(ctx, "switch_window", map[string]any{"app": "Brave"})
	if len(in.calls) != 0 {
		t.Errorf("keyboard = %v, want nothing pressed for a window already in front", in.calls)
	}
	if strings.HasPrefix(got, "error") || !strings.Contains(got, "Brave · News") {
		t.Errorf("result = %q, want it to say that window is already in front", got)
	}
}

// GNOME gives an unprivileged daemon no way to raise another application's window, so the switch is the shell's own search driven through the portal keyboard: Super, the name, Enter, in that order.
func TestExecuteTool_SwitchWindow_PressesSuperTypesTheNameAndPressesEnter(t *testing.T) {
	var in *fakeInput
	a, in := switchingAgent(t, func() (string, string) {
		if in != nil && len(in.calls) >= 3 {
			return "Brave", "News"
		}
		return "mail", "Inbox"
	})
	ctx := WithQuestion(context.Background(), "switch to Brave and read the headline")
	got := a.executeTool(ctx, "switch_window", map[string]any{"app": "Brave"})
	want := []string{"press Super", "type Brave", "press Enter"}
	if !slices.Equal(in.calls, want) {
		t.Errorf("keyboard = %v, want %v", in.calls, want)
	}
	if !strings.Contains(got, "switched to") || !strings.Contains(got, "Brave · News") {
		t.Errorf("result = %q, want it to say which window came forward", got)
	}
}

// A search that matched nothing leaves the window where it was, and the overview may still be over the screen, so one Escape closes it and the answer says plainly that nothing moved.
func TestExecuteTool_SwitchWindow_SaysWhenTheFrontWindowDidNotChange(t *testing.T) {
	a, in := switchingAgent(t, func() (string, string) { return "mail", "Inbox" })
	ctx := WithQuestion(context.Background(), "switch to Brave and read the headline")
	got := a.executeTool(ctx, "switch_window", map[string]any{"app": "Brave"})
	want := []string{"press Super", "type Brave", "press Enter", "press Escape"}
	if !slices.Equal(in.calls, want) {
		t.Errorf("keyboard = %v, want the search closed again with Escape", in.calls)
	}
	if !strings.Contains(got, "still") || !strings.Contains(got, "mail · Inbox") {
		t.Errorf("result = %q, want it to say the front window did not change", got)
	}
}

// The search matches an installed application name, not a window title, so the top result can be something else entirely; saying which window actually came forward is what keeps the next round from acting in the wrong one.
func TestExecuteTool_SwitchWindow_SaysWhenSomethingElseCameForward(t *testing.T) {
	var in *fakeInput
	a, in := switchingAgent(t, func() (string, string) {
		if in != nil && len(in.calls) >= 3 {
			return "Slack", "General"
		}
		return "mail", "Inbox"
	})
	ctx := WithQuestion(context.Background(), "switch to Brave and read the headline")
	got := a.executeTool(ctx, "switch_window", map[string]any{"app": "Brave"})
	want := []string{"press Super", "type Brave", "press Enter"}
	if !slices.Equal(in.calls, want) {
		t.Errorf("keyboard = %v, want no Escape, because activating the wrong result already closed the overview", in.calls)
	}
	if !strings.Contains(got, "Slack · General") || !strings.Contains(got, "not") || !strings.Contains(got, "Brave") {
		t.Errorf("result = %q, want it to name the window that came forward instead", got)
	}
}

// Super can be swallowed — a modal holds the grab, the compositor is busy — and the name would then be typed into whatever already had focus, a document or a compose box. Nothing is typed until the shell's own search is showing.
func TestExecuteTool_SwitchWindow_TypesNothingWhenTheOverviewNeverOpens(t *testing.T) {
	a, in := switchingAgent(t, func() (string, string) { return "mail", "Inbox" })
	// The shell's search never appears, however long it is waited for: the front window stays the user's mail all through.
	a.observe = func(ctx context.Context) (string, string, []act.Node, error) {
		return "mail", "Inbox", nil, nil
	}
	ctx := WithQuestion(context.Background(), "switch to Brave and read the headline")
	got := a.executeTool(ctx, "switch_window", map[string]any{"app": "Brave"})
	if !slices.Equal(in.calls, []string{"press Super"}) {
		t.Errorf("keyboard = %v, want Super pressed and nothing typed", in.calls)
	}
	if !strings.HasPrefix(got, "error") {
		t.Errorf("result = %q, want an error saying the desktop search never opened", got)
	}
}

// Super toggles the overview, so pressing it on an overview that is already up closes it and the name lands in whatever comes back to the front. It is pressed only when the overview is not already showing.
func TestExecuteTool_SwitchWindow_DoesNotPressSuperWhenTheOverviewIsAlreadyOpen(t *testing.T) {
	a, in := switchingAgent(t, func() (string, string) { return "mail", "Inbox" })
	a.observe = func(ctx context.Context) (string, string, []act.Node, error) {
		if slices.Contains(in.calls, "press Enter") {
			return "Brave", "News", nil, nil
		}
		return "gnome-shell", "Activities", overviewNodes(), nil
	}
	ctx := WithQuestion(context.Background(), "switch to Brave and read the headline")
	got := a.executeTool(ctx, "switch_window", map[string]any{"app": "Brave"})
	if !slices.Equal(in.calls, []string{"type Brave", "press Enter"}) {
		t.Errorf("keyboard = %v, want the name typed into the overview that was already open", in.calls)
	}
	if !strings.Contains(got, "switched to") {
		t.Errorf("result = %q, want the switch reported", got)
	}
}

// A window is named by whole words: "Mail" is not Gmail, and "Code" is not a barcode scanner. The app and the title are matched apart from each other, so a word in one never completes a name across the join between them.
func TestNamesApp_MatchesWholeWordsInTheAppAndTheTitleApart(t *testing.T) {
	cases := []struct {
		text, app string
		want      bool
	}{
		{"gmail · Inbox", "Mail", false},
		{"mail · Inbox", "Mail", true},
		{"Barcode Scanner · Home", "Code", false},
		{"Code · main.go", "code", true},
		{"brave-browser · News", "Brave", true},
		{"Slack · a Gmail thread", "Gmail", true},
		{"Visual Studio · Code review", "Visual Studio Code", false},
		{"Visual Studio Code · main.go", "visual studio code", true},
		{"switch to Brave and read the headline", "Brave", true},
		{"read me the top line", "Brave", false},
		{"anything at all", "", false},
	}
	for _, c := range cases {
		if got := namesApp(c.text, c.app); got != c.want {
			t.Errorf("namesApp(%q, %q) = %v, want %v", c.text, c.app, got, c.want)
		}
	}
}

// blockingRaiser never answers: it waits for the context it was handed to end, standing in for a gnome-shell too busy to reply. It records whether that context carried a deadline of its own.
type blockingRaiser struct{ hadDeadline bool }

func (b *blockingRaiser) Available(ctx context.Context) (bool, error) {
	_, b.hadDeadline = ctx.Deadline()
	<-ctx.Done()
	return false, ctx.Err()
}

func (b *blockingRaiser) List(ctx context.Context) ([]window.Window, error) {
	return nil, ctx.Err()
}

func (b *blockingRaiser) ByPid(ctx context.Context, pid uint32) (bool, error) {
	return false, ctx.Err()
}

func (b *blockingRaiser) ByTitle(ctx context.Context, substring string) (bool, error) {
	return false, ctx.Err()
}

func (b *blockingRaiser) ByWmClass(ctx context.Context, wmClass string) (bool, error) {
	return false, ctx.Err()
}

// The extension runs inside gnome-shell, so a wedged shell would otherwise hold the whole turn open on one window check. The three calls get a bound of their own and the switch falls through to the keys.
func TestRaiseWindow_GivesUpOnAShellThatNeverAnswers(t *testing.T) {
	was := raiserTimeout
	raiserTimeout = 20 * time.Millisecond
	t.Cleanup(func() { raiserTimeout = was })
	a, _ := switchingAgent(t, func() (string, string) { return "mail", "Inbox" })
	raiser := &blockingRaiser{}
	a.UseWindowRaiser(raiser)

	start := time.Now()
	if ok, _ := a.raiseWindow(context.Background(), "Brave"); ok {
		t.Error("raiseWindow = true, want false from an extension that never answered")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("raiseWindow took %v, want it bounded by raiserTimeout", elapsed)
	}
	if !raiser.hadDeadline {
		t.Error("the extension was called with no deadline, so a wedged shell would hold the whole turn")
	}
}

// A tool the model is never told about is a tool it never calls: the switch has to be declared, offered to a screen round, allowed through the ask gate, kept out of the log file, and counted as an action rather than a look.
func TestSwitchWindow_IsDeclaredAndCarriedByEveryScreenToolList(t *testing.T) {
	if !askAllowedTools["switch_window"] || !screenRoundTools["switch_window"] || !screenToolNames["switch_window"] || !screenActionToolNames["switch_window"] {
		t.Errorf("allowed=%v screenRound=%v screenNames=%v action=%v, want all four", askAllowedTools["switch_window"], screenRoundTools["switch_window"], screenToolNames["switch_window"], screenActionToolNames["switch_window"])
	}
	declared := false
	for _, d := range ToolDeclarations() {
		if d.Name == "switch_window" {
			declared = true
		}
	}
	if !declared {
		t.Error("switch_window is not declared, so no model can call it")
	}
	if !strings.Contains(screenTaskGuidance, "switch_window") {
		t.Error("screenTaskGuidance never mentions switch_window, so a request naming another app has nothing telling it to switch first")
	}
}

// fakeRaiser is a window raiser that records what it was asked to raise instead of moving the user's own windows, standing in for the GNOME Shell extension the daemon talks to (see internal/window). windows is what List answers with; raises says which keyed call ("pid 1234", "class Brave", "title Brave") should be reported found.
type fakeRaiser struct {
	available bool
	windows   []window.Window
	raises    map[string]bool
	calls     []string
}

func (f *fakeRaiser) Available(ctx context.Context) (bool, error) {
	if !f.available {
		return false, errors.New("the ora extension is not loaded in this shell")
	}
	return true, nil
}

func (f *fakeRaiser) List(ctx context.Context) ([]window.Window, error) {
	f.calls = append(f.calls, "list")
	return f.windows, nil
}

func (f *fakeRaiser) ByPid(ctx context.Context, pid uint32) (bool, error) {
	f.calls = append(f.calls, fmt.Sprintf("pid %d", pid))
	return f.raises[fmt.Sprintf("pid %d", pid)], nil
}

func (f *fakeRaiser) ByTitle(ctx context.Context, substring string) (bool, error) {
	f.calls = append(f.calls, "title "+substring)
	return f.raises["title "+substring], nil
}

func (f *fakeRaiser) ByWmClass(ctx context.Context, wmClass string) (bool, error) {
	f.calls = append(f.calls, "class "+wmClass)
	return f.raises["class "+wmClass], nil
}

// The pid of the process behind a window is the most exact key there is, so when List already names a window as the app asked for, that window is raised by its pid rather than by a second, looser text match — and nothing is typed over the user's screen at all.
func TestExecuteTool_SwitchWindow_RaisesByPidWhenListMatches(t *testing.T) {
	raiser := &fakeRaiser{
		available: true,
		windows:   []window.Window{{Pid: 1234, WmClass: "brave-browser", Title: "Old title"}},
		raises:    map[string]bool{"pid 1234": true},
	}
	a, in := switchingAgent(t, func() (string, string) {
		if len(raiser.calls) > 0 {
			return "Brave", "News"
		}
		return "mail", "Inbox"
	})
	a.UseWindowRaiser(raiser)
	ctx := WithQuestion(context.Background(), "switch to Brave and read the headline")
	got := a.executeTool(ctx, "switch_window", map[string]any{"app": "Brave"})
	if len(in.calls) != 0 {
		t.Errorf("keyboard = %v, want nothing pressed when the extension raised the window", in.calls)
	}
	if !slices.Equal(raiser.calls, []string{"list", "pid 1234"}) {
		t.Errorf("raiser = %v, want the list read and the matching window raised by pid", raiser.calls)
	}
	if !strings.Contains(got, "switched to") || !strings.Contains(got, "Brave · News") || !strings.Contains(got, "pid 1234") {
		t.Errorf("result = %q, want it to say which window came forward and that pid 1234 raised it", got)
	}
}

// An app with no window in the list — the extension answered but nothing there names it, or Available itself said no windows are open — falls back to the class its windows carry before the title, since a title is often a document name and not the app.
func TestExecuteTool_SwitchWindow_FallsBackToTheWindowClassWhenListIsEmpty(t *testing.T) {
	raiser := &fakeRaiser{available: true, raises: map[string]bool{"class Brave": true}}
	a, in := switchingAgent(t, func() (string, string) {
		if len(raiser.calls) > 1 {
			return "Brave", "News"
		}
		return "mail", "Inbox"
	})
	a.UseWindowRaiser(raiser)
	ctx := WithQuestion(context.Background(), "switch to Brave and read the headline")
	got := a.executeTool(ctx, "switch_window", map[string]any{"app": "Brave"})
	if !slices.Equal(raiser.calls, []string{"list", "class Brave"}) {
		t.Errorf("raiser = %v, want the empty list then the class", raiser.calls)
	}
	if len(in.calls) != 0 {
		t.Errorf("keyboard = %v, want nothing pressed when the class raised the window", in.calls)
	}
	if !strings.Contains(got, "wm_class") {
		t.Errorf("result = %q, want it to name wm_class as the key that raised it", got)
	}
}

// A window whose class says nothing about the app is still raisable by its title, tried once the class has failed.
func TestExecuteTool_SwitchWindow_FallsBackToTheWindowTitle(t *testing.T) {
	raiser := &fakeRaiser{available: true, raises: map[string]bool{"title Brave": true}}
	a, in := switchingAgent(t, func() (string, string) {
		if len(raiser.calls) > 2 {
			return "Brave", "News"
		}
		return "mail", "Inbox"
	})
	a.UseWindowRaiser(raiser)
	ctx := WithQuestion(context.Background(), "switch to Brave and read the headline")
	a.executeTool(ctx, "switch_window", map[string]any{"app": "Brave"})
	if !slices.Equal(raiser.calls, []string{"list", "class Brave", "title Brave"}) {
		t.Errorf("raiser = %v, want the list, then the class, then the title", raiser.calls)
	}
	if len(in.calls) != 0 {
		t.Errorf("keyboard = %v, want nothing pressed when the title raised the window", in.calls)
	}
}

// Until the extension is installed and the user has logged back in, the shell's own search is the only way in, so an extension that is not there falls through to the keys rather than failing the switch.
func TestExecuteTool_SwitchWindow_UsesTheKeysWhenTheExtensionIsNotThere(t *testing.T) {
	raiser := &fakeRaiser{available: false}
	var in *fakeInput
	a, in := switchingAgent(t, func() (string, string) {
		if in != nil && len(in.calls) >= 3 {
			return "Brave", "News"
		}
		return "mail", "Inbox"
	})
	a.UseWindowRaiser(raiser)
	ctx := WithQuestion(context.Background(), "switch to Brave and read the headline")
	got := a.executeTool(ctx, "switch_window", map[string]any{"app": "Brave"})
	want := []string{"press Super", "type Brave", "press Enter"}
	if !slices.Equal(in.calls, want) {
		t.Errorf("keyboard = %v, want %v", in.calls, want)
	}
	if len(raiser.calls) != 0 {
		t.Errorf("raiser = %v, want nothing asked of an extension that is not loaded", raiser.calls)
	}
	if !strings.Contains(got, "switched to") {
		t.Errorf("result = %q, want the switch reported the same way whichever path made it", got)
	}
}

// TestScreenRoundDeclarations_StayShort holds the fifteen screen-round declarations to a byte budget and checks the rules that only a declaration carries are still in one. Every screen round of a Codex or Claude ask re-sends all fifteen, so a byte here is paid on every round of every screen task; the budget is what keeps the round under the 3,000-token bound TestAskCodex_ScreenRoundsStayUnderThreeThousandTokens measures. Input: none. Output: a failure naming the total when the declarations grow back.
func TestScreenRoundDeclarations_StayShort(t *testing.T) {
	decls := trimToDeclarations((&Agent{}).askToolDeclarations(), screenRoundTools)
	if len(decls) != len(screenRoundTools) {
		t.Fatalf("%d declarations for %d screen-round tools", len(decls), len(screenRoundTools))
	}
	total, byName := 0, map[string]string{}
	for _, tool := range codexTools(decls) {
		raw, err := json.Marshal(tool)
		if err != nil {
			t.Fatalf("%s: %v", tool.Name, err)
		}
		total += len(raw)
		byName[tool.Name] = strings.ToLower(tool.Description)
	}
	// 6,450 bytes leaves the measured round at 2,989 tokens: the rest of a round (instruction, thread, the newest screen listing) is about 5,450 bytes, and 3,000 tokens is 12,000 bytes at four bytes a token. It was 6,400 until draw started taking a list of shapes instead of one, which is about 25 more tokens on every screen round; the turn that marked up a diagram on 2026-09-05 spent ten rounds and 38,335 input tokens drawing ten shapes one per round, and now spends one. That leaves the last round 11 tokens under the 3,000 bound — the next word added to any screen-round declaration fails TestAskCodex_ScreenRoundsStayUnderThreeThousandTokens.
	const screenRoundDeclarationBudget = 6450
	if total > screenRoundDeclarationBudget {
		t.Errorf("the screen-round declarations are %d bytes, want at most %d", total, screenRoundDeclarationBudget)
	}
	// Each rule is stated in exactly one declaration; the word checked for is the shortest one that phrasing cannot lose without losing the rule.
	for _, rule := range []struct{ tool, word string }{
		{"click_at", "look"},        // coordinates come only from a delivered look
		{"click", "observe_screen"}, // observe_screen after every action
		{"click", "submits"},        // the stop line on send, pay, delete, submit
		{"type_text", "secret"},     // secrets are never typed
		{"press_key", "type_text"},  // press_key for keys, type_text for text
		{"switch_window", "names"},  // only when the request names the application
	} {
		if !strings.Contains(byName[rule.tool], rule.word) {
			t.Errorf("%s's description no longer says %q: %q", rule.tool, rule.word, byName[rule.tool])
		}
	}
}

// TestDelegateTool_IsRegistered checks the delegate tool is declared, allowed for asks, and dispatched by executeTool, so a model that is offered it can actually call it.
func TestDelegateTool_IsRegistered(t *testing.T) {
	found := false
	for _, tool := range toolDefinitions() {
		for _, fd := range tool.FunctionDeclarations {
			if fd.Name == "delegate" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("delegate tool declaration not found in toolDefinitions")
	}
	if !askAllowedTools["delegate"] {
		t.Error("delegate is not in askAllowedTools, so an ask could never run it")
	}
	a := &Agent{}
	got := a.executeTool(context.Background(), "delegate", map[string]any{})
	if !strings.Contains(got, "brief") {
		t.Errorf("executeTool(delegate) = %q, want the handler's own missing-brief error rather than the unknown-tool default", got)
	}
}

// TestNewScreenScope_GivesOneCallerItsOwnNumberedList checks the exported scope a long-running job installs keeps that job's screen state to itself: the list one scope observed is not visible in another scope, nor in the agent-wide state a directly driven tool call reads. Two jobs sharing one list is how a click by number lands in the other job's window.
func TestNewScreenScope_GivesOneCallerItsOwnNumberedList(t *testing.T) {
	a := &Agent{}
	jobA := a.NewScreenScope(context.Background())
	jobB := a.NewScreenScope(context.Background())
	a.rememberScreen(jobA, []act.Item{{N: 1, Role: "push button", Label: "Play"}}, screenSnapshot{app: "Brave", title: "Netflix"})

	if got := a.seen(jobA); len(got) != 1 || got[0].Label != "Play" {
		t.Fatalf("the job's own scope holds %+v, want the list it observed", got)
	}
	if got := a.seen(jobB); len(got) != 0 {
		t.Errorf("a second scope holds %+v, want nothing of the first one's list", got)
	}
	if got := a.seen(context.Background()); len(got) != 0 {
		t.Errorf("the agent-wide state holds %+v, want a scoped call to have left it alone", got)
	}
	if got := a.lastScreen(jobB); got.app != "" {
		t.Errorf("a second scope's last screen = %+v, want the zero value", got)
	}
}

// query_memory kind=meeting reads only the meeting notes, newest first, and stops at maxMeetingNotesListed with a count of the rest, instead of paging the whole notes table into one answer.
func TestListMeetingNotes_NewestFirstAndCapped(t *testing.T) {
	b := &toolTestBrain{}
	base := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	for i := 0; i < maxMeetingNotesListed+3; i++ {
		b.notes = append(b.notes, db.Note{ID: int64(i + 1), Kind: "meeting", Content: fmt.Sprintf("# Standup %d", i+1), CreatedAt: base.Add(time.Duration(i) * time.Hour)})
	}
	b.notes = append(b.notes, db.Note{ID: 999, Kind: "fact", Content: "not a meeting", CreatedAt: base})
	a := NewAgent(nil, nil, b, nil, "")
	got := a.listMeetingNotes(context.Background(), time.Time{}, time.Time{})
	if strings.Contains(got, "not a meeting") {
		t.Errorf("a non-meeting note was listed: %s", got)
	}
	if !strings.HasPrefix(got, fmt.Sprintf("[note#%d]", maxMeetingNotesListed+3)) {
		t.Errorf("first line = %q, want the newest meeting first", strings.SplitN(got, "\n", 2)[0])
	}
	if !strings.Contains(got, "and 3 more") {
		t.Errorf("result does not count the meetings left out: %s", got)
	}
	// One line per meeting plus the closing count line; the note formatter repeats the reference inside each line, so lines are counted rather than references.
	if n := len(strings.Split(got, "\n")); n != maxMeetingNotesListed+1 {
		t.Errorf("listed %d lines, want the cap of %d plus the count line", n, maxMeetingNotesListed)
	}
}

// TestExecuteTool_OpenURL_RefusesNonHTTPSchemes checks open_url hands the desktop opener http and https links and nothing else. The url in a tool call is routinely copied out of screen text or a page the model just read, so file:///home/user/.ssh/id_rsa, javascript: and smb: all arrive here as plain strings; ipc.Open makes the same check on the same three commands.
func TestExecuteTool_OpenURL_RefusesNonHTTPSchemes(t *testing.T) {
	a := NewAgent(nil, nil, nil, nil, "")
	var opened []string
	original := openURLCommand
	openURLCommand = func(raw string) *exec.Cmd {
		opened = append(opened, raw)
		return exec.Command("sh", "-c", "exit 0")
	}
	t.Cleanup(func() { openURLCommand = original })

	for _, raw := range []string{"file:///home/user/.ssh/id_rsa", "javascript:alert(1)", "smb://share/secrets", "/etc/passwd"} {
		got := a.executeTool(context.Background(), "open_url", map[string]any{"url": raw})
		if !strings.HasPrefix(got, "error") {
			t.Errorf("open_url(%q) = %q, want a refusal", raw, got)
		}
	}
	if len(opened) != 0 {
		t.Fatalf("a refused url must never reach the opener, got %v", opened)
	}

	got := a.executeTool(context.Background(), "open_url", map[string]any{"url": "https://example.com/page"})
	if !strings.Contains(got, "https://example.com/page") {
		t.Errorf("open_url on an https link = %q, want it opened", got)
	}
	if len(opened) != 1 || opened[0] != "https://example.com/page" {
		t.Fatalf("expected the https link handed to the opener once, got %v", opened)
	}
}

// TestLiveTools_OmitsApprovalGatedToolsWithoutAnApprover checks a session with nobody reading ToolApprovalChan never declares the tools that wait on it — the daemon reads that channel nowhere, so a voice session that called one of them parked until the session ended and the model never got a result. With an approver registered (the terminal UI) the same tools are declared again.
func TestLiveTools_OmitsApprovalGatedToolsWithoutAnApprover(t *testing.T) {
	declared := func() map[string]bool {
		names := map[string]bool{}
		for _, tool := range liveTools() {
			for _, d := range tool.FunctionDeclarations {
				names[d.Name] = true
			}
		}
		return names
	}

	for name := range approvalGatedTools {
		if declared()[name] {
			t.Errorf("live tools declare %q with nobody to approve it", name)
		}
	}

	SetToolApprovals(true)
	t.Cleanup(func() { SetToolApprovals(false) })
	for name := range approvalGatedTools {
		if !declared()[name] {
			t.Errorf("live tools drop %q even though an approver is registered", name)
		}
	}
}

// TestExecuteTool_ApprovalGatedTools_RefuseInsteadOfBlocking checks that with no approver registered — every daemon path: ask, live voice, routines, act jobs — a call to one of the approval-gated tools comes back with a refusal rather than parking on ToolApprovalChan, which is what a model naming an undeclared tool would do.
func TestExecuteTool_ApprovalGatedTools_RefuseInsteadOfBlocking(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	sensitive := filepath.Join(t.TempDir(), ".ssh", "id_rsa")
	if err := os.MkdirAll(filepath.Dir(sensitive), 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(sensitive, []byte("-----BEGIN PRIVATE KEY-----"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	calls := []struct {
		name string
		args map[string]any
	}{
		{"shell_exec", map[string]any{"command": "echo hello"}},
		{"read_clipboard", map[string]any{}},
		{"read_file", map[string]any{"path": sensitive}},
	}
	for _, call := range calls {
		done := make(chan string, 1)
		go func() { done <- a.executeTool(context.Background(), call.name, call.args) }()
		select {
		case got := <-done:
			if !strings.HasPrefix(got, "error") {
				t.Errorf("%s = %q, want a refusal", call.name, got)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s blocked instead of refusing", call.name)
		}
		select {
		case req := <-a.ToolApprovalChan:
			t.Fatalf("%s put an approval request nobody can answer on the channel: %+v", call.name, req)
		default:
		}
	}
}

// TestRunShellCommand_TruncatesWithoutSplittingARune checks a long command output is cut on a rune boundary. The cut used to be result[:2000], which halves a multi-byte rune and puts an invalid string into a JSON tool response.
func TestRunShellCommand_TruncatesWithoutSplittingARune(t *testing.T) {
	got := RunShellCommand("printf %s '" + strings.Repeat("é", 2100) + "'")

	if !utf8.ValidString(got) {
		t.Error("the truncated shell output is not valid UTF-8")
	}
	if !strings.HasSuffix(got, "\n... (truncated)") {
		t.Fatalf("expected the truncation marker, got the tail %q", oratext.Runes(got, 40))
	}
	if n := utf8.RuneCountInString(strings.TrimSuffix(got, "\n... (truncated)")); n != 2000 {
		t.Errorf("kept %d runes, want 2000", n)
	}
}

// TestExecuteTool_ReadFile_TruncatesWithoutSplittingARune is the same rune-boundary check for read_file, which cut at result[:4000].
func TestExecuteTool_ReadFile_TruncatesWithoutSplittingARune(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	path := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat("é", 4100)), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got := a.executeTool(context.Background(), "read_file", map[string]any{"path": path})

	if !utf8.ValidString(got) {
		t.Error("the truncated file content is not valid UTF-8")
	}
	if n := utf8.RuneCountInString(strings.TrimSuffix(got, "\n... (truncated, file too large)")); n != 4000 {
		t.Errorf("kept %d runes, want 4000", n)
	}
}

// TestExecuteTool_WaitFor_RefusesAnUnknownKind checks a check kind outside the four wait_for declares is refused by name rather than polled for five seconds and reported as a real negative.
func TestExecuteTool_WaitFor_RefusesAnUnknownKind(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")

	got := a.executeTool(context.Background(), "wait_for", map[string]any{"kind": "spinner_gone", "value": "Loading"})

	if !strings.HasPrefix(got, "error") {
		t.Fatalf("wait_for with an unknown kind = %q, want a refusal", got)
	}
	for _, kind := range []string{"title_contains", "item_present", "item_absent", "field_holds"} {
		if !strings.Contains(got, kind) {
			t.Errorf("the refusal %q does not name %s", got, kind)
		}
	}
}
