package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"ora/internal/act"
	"ora/internal/db"
	"ora/internal/db/dbtest"
	"ora/internal/memory"
	"ora/internal/tracker"
	"ora/internal/util"
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
	notes          []db.Note
	threadEpisodes []db.Episode
	// addedTasks records what add_task wrote, and addTaskErr makes the write fail, so the tool's own reporting can be asserted without a real store.
	addedTasks      []string
	addTaskErr      error
	convTitles      []string
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

	// taskTitleID/taskTitle, taskDoneID/taskDone and deletedTaskID capture what revise's task path asked the store to do; taskErr forces all three to fail.
	taskTitleID   int64
	taskTitle     string
	taskDoneID    int64
	taskDone      bool
	deletedTaskID int64
	taskErr       error

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

func (b *toolTestBrain) CreateConversation(ctx context.Context, title, brain string) (int64, error) {
	b.convTitles = append(b.convTitles, title)
	return int64(len(b.convTitles)), nil
}

func (b *toolTestBrain) SetUserTaskTitle(ctx context.Context, id int64, title string) error {
	if b.taskErr != nil {
		return b.taskErr
	}
	b.taskTitleID, b.taskTitle = id, title
	return nil
}

func (b *toolTestBrain) SetUserTaskDone(ctx context.Context, id int64, done bool) error {
	if b.taskErr != nil {
		return b.taskErr
	}
	b.taskDoneID, b.taskDone = id, done
	return nil
}

func (b *toolTestBrain) DeleteUserTask(ctx context.Context, id int64) error {
	if b.taskErr != nil {
		return b.taskErr
	}
	b.deletedTaskID = id
	return nil
}

func (b *toolTestBrain) AddUserTask(ctx context.Context, title string, conversationID int64) (int64, error) {
	if b.addTaskErr != nil {
		return 0, b.addTaskErr
	}
	b.addedTasks = append(b.addedTasks, title)
	return int64(len(b.addedTasks)), nil
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

// TestExecuteTool_Recall_InvalidRangeAndTypeErrors verifies that malformed types or reversed date ranges produce explicit error strings rather than silent fallbacks or false-empty results.
func TestExecuteTool_Recall_InvalidRangeAndTypeErrors(t *testing.T) {
	cases := []struct {
		name      string
		args      map[string]any
		wantError string
	}{
		{
			name:      "non-string since produces error",
			args:      map[string]any{"since": float64(20260704)},
			wantError: "error",
		},
		{
			name:      "non-string until produces error",
			args:      map[string]any{"until": 12345},
			wantError: "error",
		},
		{
			name:      "reversed range produces error",
			args:      map[string]any{"since": "2026-07-10T00:00:00Z", "until": "2026-07-05T00:00:00Z"},
			wantError: "runs backwards",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			brain := &toolTestBrain{
				windowEpisodes: []db.Episode{
					{ID: 1, CreatedAt: time.Now(), App: "Mail", Title: "Inbox", ScreenText: "should not be reached"},
				},
			}
			a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")
			res := a.executeTool(context.Background(), "recall", tc.args)
			if !strings.Contains(res, tc.wantError) {
				t.Errorf("result = %q, want error containing %q", res, tc.wantError)
			}
			if strings.Contains(res, "should not be reached") {
				t.Errorf("invalid args must not fall through to default window: %q", res)
			}
		})
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

// TestExecuteTool_QueryMemory_HitFormatting verifies that query_memory formats hits consistently (including source tags, note ref_ids, excerpt truncation, and empty result sentinels).
func TestExecuteTool_QueryMemory_HitFormatting(t *testing.T) {
	overlong := strings.Repeat("x", 2000)
	cases := []struct {
		name         string
		hits         []db.MemoryHit
		wantContains []string
		mustNotHave  []string
	}{
		{
			name: "formats episode and summary hits",
			hits: []db.MemoryHit{
				{Source: "episode", Content: "saw the Riddler press conference on Gotham News"},
				{Source: "summary", Content: "spent the afternoon debugging CUDA OOM errors"},
			},
			wantContains: []string{"[episode] saw the Riddler", "[summary] spent the afternoon"},
		},
		{
			name: "formats note hit with ref id",
			hits: []db.MemoryHit{
				{Source: "note", Content: "Samara is my wife", RefID: 105},
			},
			wantContains: []string{"[note#105] Samara is my wife"},
		},
		{
			name: "truncates overlong hit content",
			hits: []db.MemoryHit{
				{Source: "summary", Content: overlong},
			},
			mustNotHave: []string{overlong},
		},
		{
			name:         "zero hits returns no memory matches sentinel",
			hits:         nil,
			wantContains: []string{"no memory"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			brain := &toolTestBrain{hybridHits: tc.hits}
			a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")
			result := a.executeTool(context.Background(), "query_memory", map[string]any{"query": "search"})
			for _, want := range tc.wantContains {
				if !strings.Contains(strings.ToLower(result), strings.ToLower(want)) {
					t.Errorf("expected result to contain %q, got: %q", want, result)
				}
			}
			for _, bad := range tc.mustNotHave {
				if strings.Contains(result, bad) {
					t.Errorf("result unexpectedly contained forbidden snippet: %q", bad)
				}
			}
		})
	}
}

// TestToolDefinitions_AllNonBlocking verifies every function declaration is declared NON_BLOCKING. Left unset, the Live API treats a declaration as BLOCKING, which makes the model stop talking and stop listening for the whole duration of a tool call — a memory lookup that takes two seconds turns into two seconds of dead air on a voice call. NON_BLOCKING lets the model keep the conversation going while the result comes back out of band (see scheduleFor for how the result is then folded in).
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

// TestExecuteTool_BadDate_SaysWhichDatesWork verifies an unparseable since is reported instead of ignored on both recall and query_memory, and that the one date error phrasing names the forms that do work, so the model can fix the argument in the same turn instead of ending the turn on a failure.
func TestExecuteTool_BadDate_SaysWhichDatesWork(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "FAKE_API_KEY")

	result := a.executeTool(context.Background(), "recall", map[string]any{"since": "last tuesdayish"})
	for _, want := range []string{"real date", "today", "yesterday", "2026-07-05"} {
		if !strings.Contains(result, want) {
			t.Errorf("expected the date error to mention %q so the model can retry, got: %q", want, result)
		}
	}

	brain := &toolTestBrain{hybridHits: []db.MemoryHit{{Source: "note", Content: "a note"}}}
	a = NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")
	result = a.executeTool(context.Background(), "query_memory", map[string]any{"query": "riddler", "since": "last tuesdayish"})
	if !strings.HasPrefix(result, "error") {
		t.Errorf(`expected an "error: ..." result for an unparseable since, got %q`, result)
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
	store := dbtest.Open(t)
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
		{CreatedAt: base.UTC(), Content: `{"task_name":"Brightpath Statement Builder VRDS","summary":"scoring vulnerability data"}`},
		{CreatedAt: base.Add(8 * time.Hour).UTC(), Content: `{"task_name":"Ora Memory Architecture Development","summary":"recall surgery"}`},
		{CreatedAt: base.Add(9 * time.Hour).UTC(), Content: `{"task_name":"Raw Activity Log","summary":"Unknown | Unknown"}`},
	}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")
	out := a.ExecuteTool(context.Background(), "recall", map[string]any{"since": "2026-08-28", "until": "2026-08-28"})
	if !strings.Contains(out, "Brightpath Statement Builder VRDS") || !strings.Contains(out, "Ora Memory Architecture Development") {
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
		{NoteID: 41, Owner: "Zemna Braxen", Text: "push the route planning branch", Status: memory.StatusOpen, Priority: memory.PriorityNormal},
	}}
	got := NewAgent(nil, nil, brain, nil, "").ExecuteTool(context.Background(), "action_items", map[string]any{})
	if !strings.Contains(got, "[note#41]") || !strings.Contains(got, "push the route planning branch") {
		t.Errorf("got %q, want the item with its id", got)
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
	a.Point = func(x, y, w, h int, label string) error {
		f.rings = append(f.rings, fmt.Sprintf("%s %d,%d %dx%d", label, x, y, w, h))
		return nil
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
	// NewAgent wires the real desktop-entry reader and the real launcher, which start real applications on whatever machine runs the tests — a test that reached open_app opened a browser window on the developer's own screen. Every test agent gets a fixed list and a launcher that records instead; a test that wants to watch a launch replaces these with its own.
	a.desktopEntries = func() map[string]string { return map[string]string{} }
	a.launchApp = func(path string) error { return fmt.Errorf("this test never wired a launcher, so nothing was started") }
	drawn = &[]string{}
	a.Draw = func(_, shape string, points [][2]int, x, y, w, h int, label string) error {
		*drawn = append(*drawn, fmt.Sprintf("%s %v %d,%d,%d,%d %q", shape, points, x, y, w, h, label))
		return nil
	}
	// Raw coordinates are read off the last look, so a test that draws from them has to be able to take one. This camera hands back the whole screen at its own size, which maps every picture point to itself and leaves such a test reading the numbers it gave.
	a.capture = func(ctx context.Context) (tracker.Capture, error) {
		return tracker.Capture{Data: []byte("fake-jpeg-bytes"), Mime: "image/jpeg", W: 1920, H: 1080, Scale: 1}, nil
	}
	// A drawing that names an element checks the number still points at it and reads where it is now, so these two seams stand in for the accessibility bus; a test that wants an element to have moved replaces them through drawingScreen.
	a.verify = func(ctx context.Context, ref, role, label string, x, y, w, h int) error { return nil }
	// The stop line on type_text and a focused key press reads whether the field the last click acted on still holds the keyboard, which is the accessibility bus again; here it always does, and a test about a focus that moved replaces this.
	a.focused = func(context.Context, string) (bool, error) { return true, nil }
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

// Most clicks succeed through doAction, and those deserve to see the pointer indicator too, not only the ones that fall back to a real press. The tap has to land at the element's rectangle read fresh, not the one observe_screen listed, since the page can have scrolled between the list and the click.
func TestExecuteTool_Click_TapsTheFreshCentreBeforeASuccessfulAction(t *testing.T) {
	a, f := ringingAgent(t, act.Node{Role: "push button", Label: "Play", X: 10, Y: 20, W: 80, H: 30, Showing: true, Ref: "r-play"})
	var taps []string
	a.Tap = func(x, y int, label string) error {
		taps = append(taps, fmt.Sprintf("%s %d,%d", label, x, y))
		return nil
	}
	tapLead = 0
	a.executeTool(context.Background(), "observe_screen", map[string]any{})
	f.at.X, f.at.Y = 14, 24
	got := a.executeTool(context.Background(), "click", map[string]any{"n": float64(1)})
	if len(taps) != 1 || taps[0] != "Play 54,39" {
		t.Errorf("taps = %v, want exactly one tap at the fresh centre (54,39), not the listed one (50,35); result %q", taps, got)
	}
	if len(f.clicked) != 1 {
		t.Errorf("clicked = %v, want the click to still go through doAction", f.clicked)
	}
}

// The pointer fallback fires when the element has no accessibility action at all; it has to click where the element is now, not where observe_screen's stale list left it, and it must not tap a second time on top of the tap the click already showed before trying doAction.
func TestExecuteTool_Click_PointerFallbackUsesTheFreshRectangle(t *testing.T) {
	a, f := ringingAgent(t, act.Node{Role: "push button", Label: "Brave", X: 10, Y: 20, W: 80, H: 30, Showing: true, Ref: "r-brave"})
	a.capture = nil
	f.readErr = nil
	a.doAction = func(ctx context.Context, ref string) (string, error) {
		return "", errors.New("the element offers no action to fire")
	}
	in := &fakeInput{}
	a.input = onceInput(func(ctx context.Context) (InputDevice, error) { return in, nil })
	var taps []string
	a.Tap = func(x, y int, label string) error {
		taps = append(taps, fmt.Sprintf("%d,%d", x, y))
		return nil
	}
	tapLead = 0
	a.executeTool(context.Background(), "observe_screen", map[string]any{})
	f.at.X, f.at.Y = 14, 24
	got := a.executeTool(context.Background(), "click", map[string]any{"n": float64(1)})
	if len(in.calls) != 1 || in.calls[0] != "click 54,39" {
		t.Errorf("pointer = %v, want one click at the fresh centre (54,39), not the listed one (50,35)", in.calls)
	}
	if len(taps) != 1 || taps[0] != "54,39" {
		t.Errorf("taps = %v, want exactly one tap, at the fresh centre, not one per attempt", taps)
	}
	if !strings.Contains(got, "via pointer") {
		t.Errorf("result = %q, want it to say the pointer did it", got)
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

// A Live voice session takes tool results as text, so a look it makes takes a picture nobody can show it. Telling it to "look first" there is advice that can never be followed: on 2026-09-07 a voice session looped look -> click_at -> refusal three times against an Electron window, then told the user its screen tools were broken and saved a note about it. The refusal has to say the picture cannot be shown and name the path that does work.
func TestExecuteTool_ClickAt_SaysWhenTheSessionCannotBeShownThePicture(t *testing.T) {
	a, _, _ := lookingAgent(t)
	ctx := withAskLookState(context.Background())
	// The look is taken but never handed over, which is exactly the state a voice session leaves it in.
	a.executeTool(ctx, "look", map[string]any{})

	got := a.executeTool(ctx, "click_at", map[string]any{"x": 10.0, "y": 20.0})
	if strings.Contains(got, "call look") {
		t.Errorf("result = %q, want it to stop telling a blind session to look again", got)
	}
	for _, want := range []string{"cannot show it to me", "observe_screen"} {
		if !strings.Contains(got, want) {
			t.Errorf("result = %q, want it to say %q", got, want)
		}
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

func (f *fakeInput) RightClickAt(x, y float64) error {
	f.calls = append(f.calls, fmt.Sprintf("right-click %.0f,%.0f", x, y))
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

// The real pointer is invisible while it works, so the overlay's own pointer flies to the point and taps there before the press lands: what the user sees is where the click is going, in the order it happens.
func TestExecuteTool_ClickAt_ShowsTheTapBeforeThePress(t *testing.T) {
	a, in := typingAgent(t)
	var order []string
	a.Tap = func(x, y int, label string) error {
		order = append(order, fmt.Sprintf("tap %d,%d", x, y))
		return nil
	}
	tapLead = 0
	ctx := lookedAt(t, a)
	a.executeTool(ctx, "click_at", map[string]any{"x": 10.0, "y": 20.0})
	order = append(order, in.calls...)
	if len(order) != 2 || order[0] != "tap 20,72" || order[1] != "click 20,72" {
		t.Errorf("sequence = %v, want the tap shown at the point, then the click there", order)
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

// Enter presses whatever has focus, and after a click at a point the tree says what that is: an entry is pressed, a Send button is stopped by name, and a window with nothing readable is pressed on the click alone. Tab moves the keyboard the same way and gets the same read.
func TestExecuteTool_PressKey_EnterAfterAPointClickIsCheckedAgainstWhatHoldsTheKeyboard(t *testing.T) {
	a, in := typingAgent(t)
	ctx := lookedAt(t, a)
	a.executeTool(ctx, "observe_screen", map[string]any{})
	a.executeTool(ctx, "click_at", map[string]any{"x": 400.0, "y": 400.0})

	holdsKeyboard(t, act.Node{Role: "push button", Label: "Send", Ref: "r-send"}, true)
	got := a.executeTool(ctx, "press_key", map[string]any{"keys": "Enter"})
	if len(in.calls) != 1 || !strings.HasPrefix(got, "Stopped before ") || !strings.Contains(got, "Send") {
		t.Errorf("keyboard = %v, result %q; want the Enter stopped by the button's name", in.calls, got)
	}

	holdsKeyboard(t, act.Node{Role: "entry", Label: "Search", Ref: "r-search"}, true)
	if got := a.executeTool(ctx, "press_key", map[string]any{"keys": "Enter"}); len(in.calls) != 2 {
		t.Errorf("keyboard = %v, result %q; want the Enter pressed on the entry", in.calls, got)
	}

	holdsKeyboard(t, act.Node{}, false)
	a.executeTool(ctx, "press_key", map[string]any{"keys": "Tab"})
	if got := a.executeTool(ctx, "press_key", map[string]any{"keys": "Enter"}); len(in.calls) != 4 {
		t.Errorf("keyboard = %v, result %q; want the Enter pressed on the click alone when nothing readable holds the keyboard", in.calls, got)
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

// Mutter closes a RemoteDesktop session when the screencast stream behind it ends (a monitor change, the screen locking, sleep), and every call on the old handle then fails with "Invalid session" until the daemon restarts. That is what killed the JBL Bluetooth click on 2026-09-08: the pointer must open a fresh session and land the action, and an unrelated error must not be retried.
func TestOnceInput_ReopensWhenThePortalSaysTheSessionIsGone(t *testing.T) {
	dead := &fakeInput{err: errors.New("Invalid session")}
	live := &fakeInput{}
	opens := 0
	open := onceInput(func(ctx context.Context) (InputDevice, error) {
		opens++
		if opens == 1 {
			return dead, nil
		}
		return live, nil
	})
	dev, err := open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := dev.ClickAt(10, 20); err != nil {
		t.Fatalf("click = %v, want it landed on a fresh session", err)
	}
	if opens != 2 || len(live.calls) != 1 || live.calls[0] != "click 10,20" {
		t.Errorf("opens = %d, live calls = %v, want one reopen and the click replayed on it", opens, live.calls)
	}
	live.err = errors.New("Invalid position")
	if err := dev.ScrollAt(1, 2, 3); err == nil || opens != 2 {
		t.Errorf("scroll err = %v, opens = %d, want the other error reported without a reopen", err, opens)
	}
	// A session granted without the devices in it answers every press this way and never recovers; it is dropped like a dead one.
	live.err = errors.New("Session is not allowed to call NotifyPointer methods")
	dev.ClickAt(1, 2)
	if opens != 3 {
		t.Errorf("opens = %d, want a device-less session dropped and reopened", opens)
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
	verifyFor := switchVerifyFor
	switchVerifyFor = 0
	t.Cleanup(func() { switchVerifyFor = verifyFor })
	return a, in
}

// A job stays in the window it started in when the request itself is about that window: a request that names the front window and not the other application is refused before a key is pressed.
func TestExecuteTool_SwitchWindow_RefusesWhenTheRequestIsAboutTheFrontWindow(t *testing.T) {
	a, in := switchingAgent(t, func() (string, string) { return "mail", "Inbox" })
	ctx := WithQuestion(context.Background(), "read me the top line in mail")
	got := a.executeTool(ctx, "switch_window", map[string]any{"app": "Brave"})
	if len(in.calls) != 0 {
		t.Errorf("keyboard = %v, want nothing pressed", in.calls)
	}
	if !strings.HasPrefix(got, "error") || !strings.Contains(got, "Brave") {
		t.Errorf("result = %q, want a refusal naming the app the request never asked for", got)
	}
}

// A request that names no window at all — "now, play it" — leaves the choice of application to the model, so the switch goes ahead. On 2026-09-07 every such request was refused and the model fell back to opening search pages it could not see.
func TestExecuteTool_SwitchWindow_AllowsWhenTheRequestNamesNoWindow(t *testing.T) {
	raiser := &fakeRaiser{
		available: true,
		windows:   []window.Window{{Pid: 42, WmClass: "spotify", Title: "Spotify"}},
		raises:    map[string]bool{"pid 42": true},
	}
	a, in := switchingAgent(t, func() (string, string) {
		if len(raiser.calls) > 0 {
			return "Spotify", "Back in Black"
		}
		return "claude-desktop", "Claude"
	})
	a.UseWindowRaiser(raiser)
	ctx := WithQuestion(context.Background(), "now, play it")
	got := a.executeTool(ctx, "switch_window", map[string]any{"app": "Spotify"})
	if len(in.calls) != 0 {
		t.Errorf("keyboard = %v, want nothing pressed when the extension raised the window", in.calls)
	}
	if strings.HasPrefix(got, "error") || !strings.Contains(got, "Spotify · Back in Black") {
		t.Errorf("result = %q, want the switch to go ahead and say what came forward", got)
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

// TestScreenRoundDeclarations_StayShort holds the screen-round declarations to a byte budget and checks the rules that only a declaration carries are still in one. Every screen round of a Codex or Claude ask re-sends all of them, so a byte here is paid on every round of every screen task; the budget is what keeps the round under the 3,000-token bound TestAskCodex_ScreenRoundsStayUnderThreeThousandTokens measures. Input: none. Output: a failure naming the total when the declarations grow back.
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
	// Raised to 6,850 on 2026-09-08 for open_app, the sixteenth: an installed application opened as itself instead of through open_url and a browser tab, which is what the Spotify ask that day failed on. About 100 tokens a round. set_budget, the seventeenth declaration briefly, is gone again (see agent.maxAskIterations, a plain hard cap in place of a self-set one), so this is back down near that mark; 6,900 leaves headroom for the wording that has moved since without reopening the budget on every future word change.
	const screenRoundDeclarationBudget = 6900
	if total > screenRoundDeclarationBudget {
		t.Errorf("the screen-round declarations are %d bytes, want at most %d", total, screenRoundDeclarationBudget)
	}
	// Each rule is stated in exactly one declaration; the word checked for is the shortest one that phrasing cannot lose without losing the rule.
	for _, rule := range []struct{ tool, word string }{
		{"click", "look"},              // a bare point's coordinates come only from a delivered look
		{"click", "observe_screen"},    // observe_screen after every action
		{"click", "submits"},           // the stop line on send, pay, delete, submit
		{"click", "then"},              // the burst is only for UI that will not survive a round trip
		{"type_text", "secret"},        // secrets are never typed
		{"press_key", "type_text"},     // press_key for keys, type_text for text
		{"open_app", "never open_url"}, // an installed application is opened, not its website
	} {
		if !strings.Contains(byName[rule.tool], rule.word) {
			t.Errorf("%s's description no longer says %q: %q", rule.tool, rule.word, byName[rule.tool])
		}
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
	got := a.listMeetingNotes(context.Background(), base, time.Time{})
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

// "Can you access the latest meeting notes" with no window listed thirty sets of minutes at 4,500 characters each on 2026-09-10, one round of 178k input tokens for an answer about one meeting. With no window the list is the newest few and a count of the rest; a window still gets the full cap.
func TestListMeetingNotes_NoWindowListsOnlyTheNewestFew(t *testing.T) {
	b := &toolTestBrain{}
	base := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		b.notes = append(b.notes, db.Note{ID: int64(i + 1), Kind: "meeting", Content: fmt.Sprintf("# Standup %d", i+1), CreatedAt: base.Add(time.Duration(i) * time.Hour)})
	}
	a := NewAgent(nil, nil, b, nil, "")
	got := a.listMeetingNotes(context.Background(), time.Time{}, time.Time{})
	if n := len(strings.Split(got, "\n")); n != latestMeetingNotesListed+1 {
		t.Errorf("listed %d lines, want %d plus the count line: %s", n, latestMeetingNotesListed, got)
	}
	if !strings.Contains(got, fmt.Sprintf("and %d more", 10-latestMeetingNotesListed)) {
		t.Errorf("result does not count the meetings left out: %s", got)
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

// xdg-open puts the page in a tab the shell keeps behind whatever is in front, so once the extension is there the browser is raised through it: the window whose class shares a word with the default browser's desktop id (brave_brave.desktop against wm_class brave-browser) is raised by pid, and the result says so. Four opens on 2026-09-07 landed behind the Claude window and the model, seeing nothing, opened the same page again.
func TestExecuteTool_OpenURL_RaisesTheBrowserThroughTheExtension(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	original, originalBrowser := openURLCommand, defaultBrowserID
	openURLCommand = func(raw string) *exec.Cmd { return exec.Command("true") }
	defaultBrowserID = func() string { return "brave_brave.desktop" }
	t.Cleanup(func() { openURLCommand, defaultBrowserID = original, originalBrowser })
	raiser := &fakeRaiser{
		available: true,
		windows:   []window.Window{{Pid: 5, WmClass: "spotify", Title: "Spotify"}, {Pid: 7, WmClass: "brave-browser", Title: "Inbox"}},
		raises:    map[string]bool{"pid 7": true},
	}
	a.UseWindowRaiser(raiser)
	got := a.executeTool(context.Background(), "open_url", map[string]any{"url": "https://example.com/page"})
	if !slices.Equal(raiser.calls, []string{"list", "pid 7"}) {
		t.Errorf("raiser = %v, want the list read and the browser raised by pid", raiser.calls)
	}
	if !strings.Contains(got, "to the front (pid 7)") {
		t.Errorf("result = %q, want it to say the browser was brought to the front", got)
	}

	// No extension, or no browser window yet: the page still opens, and the result says the window was not raised so the model does not read "opened" as "showing".
	a.UseWindowRaiser(&fakeRaiser{available: false})
	got = a.executeTool(context.Background(), "open_url", map[string]any{"url": "https://example.com/page"})
	if !strings.Contains(got, "not brought to the front") {
		t.Errorf("result without the extension = %q, want it to say the window was not brought forward", got)
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
		t.Fatalf("expected the truncation marker, got the tail %q", util.Runes(got, 40))
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

// The field the last click acted on stands in for the keyboard, and a dialog or the application itself can move the focus off it without a click of ours. The accessibility focused state says whether it still holds the keyboard, and when it does not the typing is refused instead of being checked against a control the text will not reach.
func TestExecuteTool_TypeText_RefusesWhenTheClickedFieldNoLongerHasFocus(t *testing.T) {
	a, in := typingAgent(t)
	ctx := context.Background()
	a.executeTool(ctx, "observe_screen", map[string]any{})
	a.rememberClick(ctx, act.Item{N: 1, Role: "entry", Label: "To", Ref: "r-1"})
	a.focused = func(context.Context, string) (bool, error) { return false, nil }
	holdsKeyboard(t, act.Node{Role: "push button", Label: "Send", Ref: "r-send"}, true)

	got := a.executeTool(ctx, "type_text", map[string]any{"text": "hello"})

	if len(in.calls) != 0 {
		t.Errorf("keyboard = %v, want nothing typed", in.calls)
	}
	if !strings.HasPrefix(got, "Stopped before ") || !strings.Contains(got, "no place to type") {
		t.Errorf("result = %q, want a stop naming the control that holds the keyboard", got)
	}
}

// holdsKeyboard stands in for the read of which element of the window in front holds the keyboard, for the run of one test. Input: the test, the element to answer with, and whether anything readable holds the keyboard at all. Output: none; the real read is put back when the test ends.
func holdsKeyboard(t *testing.T, n act.Node, ok bool) {
	t.Helper()
	restore := keyboardHolder
	keyboardHolder = func(context.Context) (act.Node, bool) { return n, ok }
	t.Cleanup(func() { keyboardHolder = restore })
}

// The reported failure: the user asked for music, the model clicked Brave's address bar by a point on the screen, the page moved the keyboard into its own search box, and the typing was refused because the clicked field no longer carried the focused bit. Another box to type in is where the keys legitimately land, so the text goes in and the result says which box got it.
func TestExecuteTool_TypeText_TypesIntoTheFieldThatHoldsTheKeyboard(t *testing.T) {
	a, in := typingAgent(t)
	ctx := context.Background()
	a.executeTool(ctx, "observe_screen", map[string]any{})
	a.rememberClick(ctx, act.Item{N: 1, Role: "entry", Label: "Address bar", Ref: "r-1"})
	a.focused = func(context.Context, string) (bool, error) { return false, nil }
	holdsKeyboard(t, act.Node{Role: "entry", Label: "Search", Ref: "r-search"}, true)

	got := a.executeTool(ctx, "type_text", map[string]any{"text": "lofi beats"})

	if len(in.calls) != 1 || in.calls[0] != "type lofi beats" {
		t.Errorf("keyboard = %v, want the text typed", in.calls)
	}
	if !strings.Contains(got, "Search") {
		t.Errorf("result = %q, want it to name the field the text went into", got)
	}
}

// A window that publishes nothing readable says nothing about where the keyboard is, and refusing on that is what stopped the typing this stop line exists to let through. It goes ahead on the remembered click, as it does when the read of the clicked field itself fails.
func TestExecuteTool_TypeText_TypesWhenNothingReadableHoldsTheKeyboard(t *testing.T) {
	a, in := typingAgent(t)
	ctx := context.Background()
	a.executeTool(ctx, "observe_screen", map[string]any{})
	a.rememberClick(ctx, act.Item{N: 1, Role: "entry", Label: "Address bar", Ref: "r-1"})
	a.focused = func(context.Context, string) (bool, error) { return false, nil }
	holdsKeyboard(t, act.Node{}, false)

	got := a.executeTool(ctx, "type_text", map[string]any{"text": "hello"})

	if len(in.calls) != 1 || in.calls[0] != "type hello" {
		t.Errorf("keyboard = %v, want the text typed", in.calls)
	}
	if strings.HasPrefix(got, "Stopped before ") {
		t.Errorf("result = %q, want the typing to go through", got)
	}
}

// The keyboard being in a box to type in is not enough on its own: a password box is one of those, and the secret stop line is checked against whichever field the keys are really going to, not only against the one the last click acted on.
func TestExecuteTool_TypeText_RefusesWhenAPasswordFieldHoldsTheKeyboard(t *testing.T) {
	a, in := typingAgent(t)
	ctx := context.Background()
	a.executeTool(ctx, "observe_screen", map[string]any{})
	a.rememberClick(ctx, act.Item{N: 1, Role: "entry", Label: "Email", Ref: "r-1"})
	a.focused = func(context.Context, string) (bool, error) { return false, nil }
	holdsKeyboard(t, act.Node{Role: "password text", Ref: "r-pass"}, true)

	got := a.executeTool(ctx, "type_text", map[string]any{"text": "hunter2"})

	if len(in.calls) != 0 {
		t.Errorf("keyboard = %v, want nothing typed", in.calls)
	}
	if !strings.HasPrefix(got, "Stopped before ") || !strings.Contains(got, "password") {
		t.Errorf("result = %q, want the refusal that never types a secret", got)
	}
}

// The same read the other way round: the clicked field still holds the keyboard, so the text goes in as before.
func TestExecuteTool_TypeText_TypesWhenTheClickedFieldStillHasFocus(t *testing.T) {
	a, in := typingAgent(t)
	ctx := context.Background()
	a.executeTool(ctx, "observe_screen", map[string]any{})
	a.rememberClick(ctx, act.Item{N: 1, Role: "entry", Label: "To", Ref: "r-1"})
	var asked []string
	a.focused = func(_ context.Context, ref string) (bool, error) { asked = append(asked, ref); return true, nil }

	got := a.executeTool(ctx, "type_text", map[string]any{"text": "hello"})

	if len(in.calls) != 1 || in.calls[0] != "type hello" {
		t.Errorf("keyboard = %v, want the text typed", in.calls)
	}
	if !slices.Equal(asked, []string{"r-1"}) {
		t.Errorf("the focus read was asked about %v, want the clicked field's own reference", asked)
	}
	if strings.HasPrefix(got, "Stopped before ") {
		t.Errorf("result = %q, want the typing to go through", got)
	}
}

// Enter on a focused Send button sends the message as surely as clicking it, so it stops at the same line — and so must the chords that press the focused control in the applications this engine drives: Ctrl+Enter is Send in Slack, Teams and Gmail.
func TestExecuteTool_PressKey_StopsBeforeSendChordsOnASendButton(t *testing.T) {
	for _, keys := range []string{"Enter", "Ctrl+Enter", "Ctrl+Return", "Shift+Enter", "Super+Enter"} {
		a, in := typingAgent(t)
		a.executeTool(context.Background(), "observe_screen", map[string]any{})
		a.rememberClick(context.Background(), act.Item{N: 2, Role: "push button", Label: "Send", Ref: "r-2"})

		got := a.executeTool(context.Background(), "press_key", map[string]any{"keys": keys})

		if len(in.calls) != 0 {
			t.Errorf("%s: keyboard = %v, want nothing pressed", keys, in.calls)
		}
		if !strings.HasPrefix(got, "Stopped before ") || !strings.Contains(got, "yes, send it") {
			t.Errorf("%s: result = %q, want a stop naming what to say to unlock it", keys, got)
		}
	}
}

// scroll_to resolves a number off a list that may be several rounds old, so it runs the same staleness check click and point_at do rather than scrolling to whatever the toolkit has since put behind that object path and reporting it as the element the list named.
func TestExecuteTool_ScrollTo_RefusesAStaleElement(t *testing.T) {
	a, _ := observingAgent(t)
	var scrolled []string
	a.scrollTo = func(_ context.Context, ref string) error { scrolled = append(scrolled, ref); return nil }
	a.verify = func(context.Context, string, string, string, int, int, int, int) error {
		return errors.New(`it is now a link "Settings"`)
	}
	ctx := context.Background()
	a.executeTool(ctx, "observe_screen", map[string]any{})

	got := a.executeTool(ctx, "scroll_to", map[string]any{"n": 1.0})

	if len(scrolled) != 0 {
		t.Errorf("scrolled to %v, want nothing scrolled to a stale element", scrolled)
	}
	if !strings.HasPrefix(got, "error") || !strings.Contains(got, "not what observe_screen listed") {
		t.Errorf("result = %q, want the staleness refusal", got)
	}
}

// "Put that on my list" writes a task, not a note. There was no tool that made one, so the model's only move was save_note — which files a fact nothing shows in Tasks, and then it said it had put the thing on the list.
func TestAddTask_WritesATaskAndSaysSo(t *testing.T) {
	b := &toolTestBrain{}
	a := NewAgent(nil, nil, b, nil, "")

	out := a.executeTool(t.Context(), "add_task", map[string]any{"title": "  add source pdfs to the excel files  "})

	if len(b.addedTasks) != 1 || b.addedTasks[0] != "add source pdfs to the excel files" {
		t.Fatalf("tasks written = %v, want the one title, trimmed", b.addedTasks)
	}
	if !strings.Contains(out, "add source pdfs to the excel files") {
		t.Errorf("tool said %q, want it to name the task it wrote", out)
	}
	if len(b.notes) != 0 {
		t.Errorf("it also wrote %d notes; a task is not a note", len(b.notes))
	}
}

// A failed write is reported as failed. Saying "added" over a write that did not happen is the whole complaint.
func TestAddTask_SaysWhenTheWriteFailed(t *testing.T) {
	b := &toolTestBrain{addTaskErr: errors.New("disk full")}
	a := NewAgent(nil, nil, b, nil, "")

	out := a.executeTool(t.Context(), "add_task", map[string]any{"title": "send the invoice"})

	if !strings.HasPrefix(out, "error") {
		t.Errorf("tool said %q, want an error the model cannot read as success", out)
	}
}

// "open spotify, and play back in black" on 2026-09-08 went to open_url and the web player, and then failed on the browser's tab titles. An installed application is opened as itself: its desktop entry is launched, its window waited for, and that window brought to the front.
func TestExecuteTool_OpenApp_LaunchesTheDesktopEntryAndRaisesItsWindow(t *testing.T) {
	raiser := &fakeRaiser{available: true, raises: map[string]bool{"pid 42": true}}
	a, in := switchingAgent(t, func() (string, string) {
		if len(raiser.calls) > 0 {
			return "Spotify", "Spotify Premium"
		}
		return "claude-desktop", "Claude"
	})
	a.UseWindowRaiser(raiser)
	a.desktopEntries = func() map[string]string {
		return map[string]string{"/apps/spotify_spotify.desktop": "Spotify", "/apps/brave_brave.desktop": "Brave"}
	}
	var launched []string
	a.launchApp = func(path string) error {
		launched = append(launched, path)
		raiser.windows = []window.Window{{Pid: 42, WmClass: "spotify", Title: "Spotify Premium"}}
		return nil
	}
	launchPoll = 0
	got := a.executeTool(WithQuestion(context.Background(), "open spotify"), "open_app", map[string]any{"app": "spotify"})
	if len(launched) != 1 || launched[0] != "/apps/spotify_spotify.desktop" {
		t.Errorf("launched = %v, want Spotify's own desktop entry", launched)
	}
	if len(in.calls) != 0 || strings.HasPrefix(got, "error") || !strings.Contains(got, "Spotify · Spotify Premium") {
		t.Errorf("result = %q, keyboard = %v, want the window raised and named, nothing typed", got, in.calls)
	}
	if got := a.executeTool(context.Background(), "open_app", map[string]any{"app": "Figma"}); !strings.Contains(got, "no installed application") {
		t.Errorf("unknown app = %q, want it said plainly", got)
	}
}

// The name the user says is rarely the entry's exact name: the whole name wins over a part of one, and the part matches case-blind.
func TestPickDesktopEntry(t *testing.T) {
	entries := map[string]string{"/a/spotify_spotify.desktop": "Spotify", "/a/spotify-tray.desktop": "Spotify Tray Helper", "/a/brave.desktop": "Brave Web Browser"}
	if got := pickDesktopEntry(entries, "spotify"); got != "/a/spotify_spotify.desktop" {
		t.Errorf("spotify = %q, want the exact name over the helper", got)
	}
	if got := pickDesktopEntry(entries, "brave"); got != "/a/brave.desktop" {
		t.Errorf("brave = %q, want the entry whose name contains it", got)
	}
	if got := pickDesktopEntry(entries, "figma"); got != "" {
		t.Errorf("figma = %q, want nothing", got)
	}
}

// Spotify, opened plain on 2026-09-08, showed observe_screen nothing: a Chromium-based application builds no accessibility tree on this desk unless it is started with the flag. One is known by the pak file beside its binary and is run from its own Exec line with the flag added; anything else goes through gio launch untouched.
func TestLaunchEntry_AddsTheAccessibilityFlagToAChromiumApp(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "player")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$(dirname \"$0\")/argv\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, chromiumMarker), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(dir, "player.desktop")
	if err := os.WriteFile(entry, []byte("[Desktop Entry]\nName=Player\nExec="+bin+" --quiet %U\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := launchEntry(entry); err != nil {
		t.Fatal(err)
	}
	var argv []byte
	for i := 0; i < 50 && len(argv) == 0; i++ {
		time.Sleep(20 * time.Millisecond)
		argv, _ = os.ReadFile(filepath.Join(dir, "argv"))
	}
	if got := strings.TrimSpace(string(argv)); got != "--quiet\n"+accessibilityFlag {
		t.Errorf("argv = %q, want the entry's own arguments, the placeholder dropped, and the flag added", got)
	}
	if isChromium("/bin/sh") {
		t.Error("a binary with no pak file beside it must not count as Chromium")
	}
}

// Spotify was already running, started plain, when open_app raised it on 2026-09-08, and the model was handed an empty listing with no reason. The reason and the two ways on are said with the raise, and the entry is patched so the next launch reads.
func TestExecuteTool_OpenApp_SaysWhenARunningChromiumAppHasNoTree(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(dir, "player")
	os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(filepath.Join(dir, chromiumMarker), nil, 0o644)
	entry := filepath.Join(dir, "player.desktop")
	os.WriteFile(entry, []byte("[Desktop Entry]\nName=Player\nExec="+bin+" %U\n"), 0o644)
	raiser := &fakeRaiser{available: true, windows: []window.Window{{Pid: 42, WmClass: "player", Title: "Player"}}, raises: map[string]bool{"pid 42": true}}
	a, _ := switchingAgent(t, func() (string, string) { return "Player", "Player" })
	a.UseWindowRaiser(raiser)
	a.desktopEntries = func() map[string]string { return map[string]string{entry: "Player"} }
	processArgs = func(pid uint32) string { return bin }
	got := a.executeTool(context.Background(), "open_app", map[string]any{"app": "Player"})
	if !strings.Contains(got, "without accessibility support") {
		t.Errorf("result = %q, want the empty listing explained", got)
	}
	if !strings.Contains(got, "next time") && !strings.Contains(got, "next launch") {
		t.Errorf("result = %q, want it to say the app reads from the next launch on", got)
	}
	patched, err := os.ReadFile(filepath.Join(home, ".local/share/applications", "player.desktop"))
	if err != nil || !strings.Contains(string(patched), accessibilityFlag) {
		t.Errorf("patched copy = %q, err %v, want a copy in the user's own applications directory carrying the flag", patched, err)
	}
	processArgs = func(pid uint32) string { return bin + " " + accessibilityFlag }
	if got := a.executeTool(context.Background(), "open_app", map[string]any{"app": "Player"}); strings.Contains(got, "without accessibility") {
		t.Errorf("result = %q, want no note for an app started with the flag", got)
	}
}

// A Chromium desktop entry gets a patched copy in the user's own applications directory, with the flag added to Exec under the main entry and under every desktop action, since XDG resolves that directory before /usr/share, /var/lib/snapd or /var/lib/flatpak, so a click on the icon then launches with the flag. Every other line, and the %U placeholder, survive untouched.
func TestPatchAccessibility_WritesTheFlagIntoTheUserCopyAndItsActions(t *testing.T) {
	src := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(src, "player")
	os.WriteFile(bin, nil, 0o755)
	os.WriteFile(filepath.Join(src, chromiumMarker), nil, 0o644)
	entry := filepath.Join(src, "player.desktop")
	original := "[Desktop Entry]\nName=Player\nExec=" + bin + " %U\nActions=NewWindow\n\n[Desktop Action NewWindow]\nName=New Window\nExec=" + bin + " --new-window %U\n"
	os.WriteFile(entry, []byte(original), 0o644)

	patchAccessibility(entry)

	dest := filepath.Join(home, ".local/share/applications", "player.desktop")
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read patched copy: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(got), "\n"), "\n")
	var execLines []string
	for _, l := range lines {
		if strings.HasPrefix(l, "Exec=") {
			execLines = append(execLines, l)
		}
	}
	want := []string{
		"Exec=" + bin + " %U " + accessibilityFlag,
		"Exec=" + bin + " --new-window %U " + accessibilityFlag,
	}
	if len(execLines) != 2 || execLines[0] != want[0] || execLines[1] != want[1] {
		t.Errorf("Exec lines = %v, want %v: the flag on both, the %%U placeholder kept", execLines, want)
	}
	if !strings.Contains(string(got), "Name=Player") || !strings.Contains(string(got), "Name=New Window") || !strings.Contains(string(got), "Actions=NewWindow") {
		t.Errorf("patched copy = %q, want every non-Exec line preserved", got)
	}
}

// Running the patch twice must do nothing the second time, and a file at the destination that Ora did not write - no user hand-edited a copy there, say - must never be overwritten.
func TestPatchAccessibility_IsIdempotentAndNeverOverwritesAForeignFile(t *testing.T) {
	src := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(src, "player")
	os.WriteFile(bin, nil, 0o755)
	os.WriteFile(filepath.Join(src, chromiumMarker), nil, 0o644)
	entry := filepath.Join(src, "player.desktop")
	os.WriteFile(entry, []byte("[Desktop Entry]\nName=Player\nExec="+bin+" %U\n"), 0o644)

	patchAccessibility(entry)
	dest := filepath.Join(home, ".local/share/applications", "player.desktop")
	first, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read patched copy: %v", err)
	}

	patchAccessibility(entry)
	second, err := os.ReadFile(dest)
	if err != nil || string(second) != string(first) {
		t.Errorf("second patch changed the file: got %q, want it unchanged from %q", second, first)
	}

	// An entry that already carries the flag must not be rewritten at all.
	alreadyFlagged := filepath.Join(src, "flagged.desktop")
	os.WriteFile(alreadyFlagged, []byte("[Desktop Entry]\nName=Flagged\nExec="+bin+" "+accessibilityFlag+" %U\n"), 0o644)
	patchAccessibility(alreadyFlagged)
	if _, err := os.Stat(filepath.Join(home, ".local/share/applications", "flagged.desktop")); !os.IsNotExist(err) {
		t.Error("an entry that already carries the flag must not get a patched copy")
	}

	// A file at the destination that Ora did not write, marked by carrying no Ora marker, must survive untouched.
	foreign := filepath.Join(src, "foreign.desktop")
	os.WriteFile(foreign, []byte("[Desktop Entry]\nName=Foreign\nExec="+bin+" %U\n"), 0o644)
	foreignDest := filepath.Join(home, ".local/share/applications", "foreign.desktop")
	os.MkdirAll(filepath.Dir(foreignDest), 0o755)
	os.WriteFile(foreignDest, []byte("hand-edited by the user, not Ora"), 0o644)
	// The Teams PWA case: open_app told the model "the application has been patched" whatever happened here, and the model passed the promise on.
	if patchAccessibility(foreign) {
		t.Error("a patch that left a foreign file in place reported the application as patched")
	}
	if !patchAccessibility(entry) {
		t.Error("Ora's own patched copy reported as not patched")
	}
	if got, _ := os.ReadFile(foreignDest); string(got) != "hand-edited by the user, not Ora" {
		t.Errorf("foreign file = %q, want it left untouched", got)
	}
}

// A snap's "current" is a symlink to its revision, and a directory walk does not step through a symlink at its root, so the marker under it was never seen and the Spotify snap was launched without its accessibility flag on 2026-09-08.
func TestIsChromium_StepsThroughASymlinkedRoot(t *testing.T) {
	dir := t.TempDir()
	rev := filepath.Join(dir, "99", "usr", "share", "player")
	if err := os.MkdirAll(rev, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(rev, chromiumMarker), nil, 0o644)
	if err := os.Symlink(filepath.Join(dir, "99"), filepath.Join(dir, "current")); err != nil {
		t.Fatal(err)
	}
	if !isChromiumUnder(filepath.Join(dir, "current")) {
		t.Error("the marker under the symlinked root should be found")
	}
}

// A miss on open_app names the installed applications that share a word with what was asked, so the model can pick one or see that nothing of that kind is installed; "no installed application is named" alone sent it guessing names on 2026-09-09.
func TestExecuteTool_OpenApp_AMissNamesTheNearestInstalledApps(t *testing.T) {
	a, _ := switchingAgent(t, func() (string, string) { return "Claude", "Claude" })
	a.desktopEntries = func() map[string]string {
		return map[string]string{"/apps/shot.desktop": "Take a Screenshot", "/apps/spotify.desktop": "Spotify", "/apps/obs.desktop": "OBS Studio"}
	}
	got := a.executeTool(context.Background(), "open_app", map[string]any{"app": "screenshot tool"})
	if !strings.HasPrefix(got, "error") || !strings.Contains(got, "Take a Screenshot") || strings.Contains(got, "Spotify") {
		t.Errorf("result = %q, want the miss to name the one installed app that shares a word and no other", got)
	}
	got = a.executeTool(context.Background(), "open_app", map[string]any{"app": "Figma"})
	if !strings.Contains(got, "3 installed") {
		t.Errorf("result = %q, want a miss with no near name to say how many applications are installed", got)
	}
}

// A launched application's window is found by what appeared, not by the word the user used: "Files" is org.gnome.Nautilus with a window called "Home", and on 2026-09-09 open_app waited 30 seconds beside that open window and said none showed.
func TestExecuteTool_OpenApp_FindsTheLaunchedWindowByWhatAppeared(t *testing.T) {
	raiser := &fakeRaiser{available: true, raises: map[string]bool{"pid 77": true}, windows: []window.Window{{Pid: 1, WmClass: "claude-desktop", Title: "Claude"}}}
	a, _ := switchingAgent(t, func() (string, string) {
		if len(raiser.calls) > 0 {
			return "org.gnome.Nautilus", "Home"
		}
		return "claude-desktop", "Claude"
	})
	a.UseWindowRaiser(raiser)
	a.desktopEntries = func() map[string]string { return map[string]string{"/apps/org.gnome.Nautilus.desktop": "Files"} }
	a.launchApp = func(string) error {
		raiser.windows = append(raiser.windows, window.Window{Pid: 77, WmClass: "org.gnome.Nautilus", Title: "Home"})
		return nil
	}
	launchPoll = 0
	got := a.executeTool(context.Background(), "open_app", map[string]any{"app": "Files"})
	if strings.HasPrefix(got, "error") || !strings.Contains(got, "pid 77") {
		t.Errorf("result = %q, want the new window raised by its pid", got)
	}
}

// Measured 2026-09-09 03:19: a click on YouTube's Play button was refused because the title had grown
// "- Audio playing" since observe_screen listed it, though the same page and button were still there.
// The same application in front with only the title changed must fall through to the element check
// (stillThere) instead of being refused on the title alone; a different application must still refuse.
func TestExecuteTool_Click_SameAppTitleChange(t *testing.T) {
	t.Run("same app, changed title: falls through to the element check and clicks", func(t *testing.T) {
		a, f := ringingAgent(t, act.Node{Role: "push button", Label: "Play (k)", X: 10, Y: 20, W: 80, H: 30, Showing: true, Ref: "r-play"})
		calls := 0
		a.observe = func(ctx context.Context) (string, string, []act.Node, error) {
			calls++
			if calls == 1 {
				return "brave", "YouTube", []act.Node{f.at}, nil
			}
			return "brave", "YouTube - Audio playing", []act.Node{f.at}, nil
		}
		a.executeTool(context.Background(), "observe_screen", map[string]any{})
		got := a.executeTool(context.Background(), "click", map[string]any{"n": float64(1)})
		if len(f.clicked) != 1 || f.clicked[0] != "r-play" {
			t.Errorf("clicked = %v, want the click to go through once the element itself still verifies; result=%q", f.clicked, got)
		}
		if !strings.Contains(got, "Play") {
			t.Errorf("result = %q, want the clicked item named", got)
		}
	})

	t.Run("different app: still refused", func(t *testing.T) {
		a, f := ringingAgent(t, act.Node{Role: "push button", Label: "Play (k)", X: 10, Y: 20, W: 80, H: 30, Showing: true, Ref: "r-play"})
		calls := 0
		a.observe = func(ctx context.Context) (string, string, []act.Node, error) {
			calls++
			if calls == 1 {
				return "brave", "YouTube", []act.Node{f.at}, nil
			}
			return "gnome-shell", "Activities", nil, nil
		}
		a.executeTool(context.Background(), "observe_screen", map[string]any{})
		got := a.executeTool(context.Background(), "click", map[string]any{"n": float64(1)})
		if len(f.clicked) != 0 {
			t.Errorf("clicked = %v, want no click once a different application came to front", f.clicked)
		}
		if !strings.Contains(got, "gnome-shell") || !strings.Contains(got, "YouTube") {
			t.Errorf("result = %q, want it to name both the window now in front and the one the list came from", got)
		}
	})
}

// TestRaiseWindow_PrefersTheAppsOwnWindowOverATabNamedAfterIt is the Spotify bug from 2026-09-11: a Chromium window showing the Spotify web player is titled "Spotify Premium", so it matched the name "Spotify" before the real Spotify window did and was raised instead. A window's WM_CLASS is the application; its title is the document or page, which can be named after anything.
func TestRaiseWindow_PrefersTheAppsOwnWindowOverATabNamedAfterIt(t *testing.T) {
	raiser := &fakeRaiser{
		available: true,
		windows: []window.Window{
			{Pid: 100, WmClass: "chromium", Title: "Spotify Premium"},
			{Pid: 200, WmClass: "spotify", Title: "Spotify"},
		},
		raises: map[string]bool{"pid 100": true, "pid 200": true},
	}
	a := &Agent{}
	a.UseWindowRaiser(raiser)

	ok, how := a.raiseWindow(context.Background(), "Spotify")
	if !ok {
		t.Fatal("no window was raised at all")
	}
	if how != "pid 200" {
		t.Errorf("raised %s, want pid 200, the window whose WM_CLASS is spotify", how)
	}
}

// TestFrontIsApp_DoesNotReadATabTitleAsTheApplication guards the other half of the same bug: "Chromium · Spotify Premium" is Chromium in front, not Spotify, so open_app must not report Spotify as already there and skip starting it.
func TestFrontIsApp_DoesNotReadATabTitleAsTheApplication(t *testing.T) {
	for _, c := range []struct {
		front, app string
		want       bool
	}{
		{"Chromium · Spotify Premium", "Spotify", false},
		{"Spotify · Daily Mix 1", "Spotify", true},
		{"Spotify", "Spotify", true},
		{"Brave Browser · Feed | LinkedIn - Brave", "Brave", true},
	} {
		if got := frontIsApp(c.front, c.app); got != c.want {
			t.Errorf("frontIsApp(%q, %q) = %v, want %v", c.front, c.app, got, c.want)
		}
	}
}

// TestOpenApp_DoesNotStartASecondCopyWhenTheWindowIsAlreadyOpen is the "why does it keep opening tabs" case: raising the window failed for some reason of the shell's, so open_app fell through to launching, and a browser that is already running answers a second invocation by opening another window. When the extension can see a window of that application, launching is the wrong move whatever the raise did.
func TestOpenApp_DoesNotStartASecondCopyWhenTheWindowIsAlreadyOpen(t *testing.T) {
	a, _ := switchingAgent(t, func() (string, string) { return "mail", "Inbox" })
	// Stubbed before anything else: the real launcher starts a real application on the machine running the test.
	var launched []string
	a.launchApp = func(path string) error { launched = append(launched, path); return nil }
	a.desktopEntries = func() map[string]string { return map[string]string{"/apps/brave.desktop": "Brave"} }
	raiser := &fakeRaiser{
		available: true,
		windows:   []window.Window{{Pid: 300, WmClass: "brave-browser", Title: "New Tab"}},
		raises:    map[string]bool{}, // every activation fails, which is what sends open_app to the launch path
	}
	a.UseWindowRaiser(raiser)

	out := a.openApp(context.Background(), "Brave")

	if !strings.Contains(out, "already open") {
		t.Errorf("open_app said %q, want it to say the window is already open rather than starting another", out)
	}
	if len(launched) > 0 {
		t.Errorf("open_app launched a second copy: %v", launched)
	}
}

// --- revise on a task the user keeps on their list ---

// The last conversation of 2026-09-12 broke on this three times. Ora added a research task, the user asked for the right context to be put on it, then asked for it to be deleted, and every attempt was refused: "revise only handles note and thread refs, not \"task\"". The revise tool's own description had been promising action-item support all along, and an action item is a note; a row on the Tasks screen is not, and nothing could touch one.
func TestReviseOnATaskTheUserKeeps(t *testing.T) {
	t.Run("rewords it", func(t *testing.T) {
		b := &toolTestBrain{}
		a := NewAgent(nil, nil, b, nil, "")
		got := a.executeTool(context.Background(), "revise", map[string]any{"ref": "task#3", "content": "research fly brain training: compute, timeline, links"})
		if got != "updated" {
			t.Fatalf("revise = %q, want it to reword the task", got)
		}
		if b.taskTitleID != 3 || b.taskTitle != "research fly brain training: compute, timeline, links" {
			t.Errorf("store saw id %d title %q, want the ref's id and the new words", b.taskTitleID, b.taskTitle)
		}
	})

	t.Run("removes it, which is what a dropped task means with no dropped column to put it in", func(t *testing.T) {
		for _, args := range []map[string]any{
			{"ref": "task#3", "remove": true},
			{"ref": "task#3", "state": "dropped"},
		} {
			b := &toolTestBrain{}
			a := NewAgent(nil, nil, b, nil, "")
			if got := a.executeTool(context.Background(), "revise", args); got != "deleted" {
				t.Errorf("revise %v = %q, want the task gone", args, got)
			}
			if b.deletedTaskID != 3 {
				t.Errorf("revise %v deleted task %d, want 3", args, b.deletedTaskID)
			}
		}
	})

	t.Run("ticks it done and opens it again", func(t *testing.T) {
		for _, tc := range []struct {
			state string
			done  bool
		}{{"done", true}, {"open", false}} {
			b := &toolTestBrain{}
			a := NewAgent(nil, nil, b, nil, "")
			if got := a.executeTool(context.Background(), "revise", map[string]any{"ref": "task#3", "state": tc.state}); got != "updated" {
				t.Errorf("revise state %q = %q, want updated", tc.state, got)
			}
			if b.taskDoneID != 3 || b.taskDone != tc.done {
				t.Errorf("state %q set task %d done=%v, want 3 done=%v", tc.state, b.taskDoneID, b.taskDone, tc.done)
			}
		}
	})

	t.Run("says so plainly when the id names nothing", func(t *testing.T) {
		b := &toolTestBrain{taskErr: errors.New("no task with id 9")}
		a := NewAgent(nil, nil, b, nil, "")
		got := a.executeTool(context.Background(), "revise", map[string]any{"ref": "task#9", "content": "whatever"})
		if !strings.HasPrefix(got, "error") {
			t.Errorf("revise = %q, want an error the model can act on", got)
		}
	})
}

// add_task gave back only the words it had filed, so a correction a moment later had no id to aim at. On 2026-09-12 the model guessed "note#2", which was a real note belonging to something else entirely, and the write was refused for the right reason by luck rather than design.
func TestAddTaskHandsBackTheRefToReviseIt(t *testing.T) {
	b := &toolTestBrain{}
	a := NewAgent(nil, nil, b, nil, "")
	got := a.executeTool(context.Background(), "add_task", map[string]any{"title": "research the fly brain"})
	if !strings.Contains(got, "task#1") {
		t.Errorf("add_task = %q, want it to name the ref revise takes", got)
	}
	if !strings.Contains(got, "research the fly brain") {
		t.Errorf("add_task = %q, want the title in it too", got)
	}
}

// --- do ---
//
// A chain of several actions said out loud — open Spotify and play this, then open Teams and message someone, then look for new messages — was driven one raw tool call at a time inside the live conversation before this tool existed.
// The job runner in internal/actjob plans first, checks each step against the change it expected, budgets itself at twice its own estimate and reads what this machine did the last few times it was asked something similar, but it was reachable only through POST /act, which only the window posts to.
// So a five-part request spoken aloud got no plan, no verification and no memory of the last run: in the session of 2026-09-12 21:19 a request to read one web page took three and a half minutes and the user brought the window forward himself.
// do is the voice session's way into that runner.

func TestExecuteTool_Do_HandsTheGoalToTheJobRunnerAndReportsWhatItSaid(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	goal := "open spotify and play Teenage Dream, then open teams and message Vexil that I am running late"
	var asked string
	a.RunJob = func(ctx context.Context, g string) (string, error) {
		asked = g
		return "done: Teenage Dream is playing and the message to Vexil is sitting in a draft", nil
	}
	result := a.executeTool(context.Background(), "do", map[string]any{"goal": goal})
	if asked != goal {
		t.Errorf("the runner was given %q, want the goal as spoken: %q", asked, goal)
	}
	if !strings.Contains(result, "the message to Vexil is sitting in a draft") {
		t.Errorf("do returned %q, want what the job said when it ended", result)
	}
}

// A do that never reached a runner, or whose job failed, must say so. Told "started", the model tells the user their chain is running and then answers questions about a job that does not exist.
func TestExecuteTool_Do_NeverClaimsAChainIsRunningWhenItIsNot(t *testing.T) {
	for _, tc := range []struct {
		name   string
		runner func(context.Context, string) (string, error)
		args   map[string]any
	}{
		{"no runner wired to this session", nil, map[string]any{"goal": "open spotify and play something"}},
		{"the runner refused the job", func(context.Context, string) (string, error) { return "", errors.New("another job is already running") }, map[string]any{"goal": "open spotify and play something"}},
		{"no goal to work on", func(context.Context, string) (string, error) { return "done: nothing", nil }, map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
			a.RunJob = tc.runner
			result := a.executeTool(context.Background(), "do", tc.args)
			if toolOutcome(result) != "error" {
				t.Errorf("do returned %q, want an error the model can read", result)
			}
		})
	}
}

// A toggle renames itself the instant it is pressed, and pressing it again must not be refused for that.
// Play becomes Pause, Mute becomes Unmute, on the same element with the same reference. The staleness check treated the new name as evidence the number now pointed at something else and sent the model back to observe_screen: on 2026-09-11 that cost three rounds and a full re-listing to press one button. The name is still checked, because the stop line is judged on it — it is judged on what the element says now.
func TestExecuteTool_Click_AToggleThatRenamedItselfIsStillTheSameButton(t *testing.T) {
	a, f := ringingAgent(t, act.Node{Role: "push button", Label: "Mute", X: 10, Y: 20, W: 60, H: 30, Ref: "btn", Showing: true})
	ctx := context.Background()
	a.executeTool(ctx, "observe_screen", map[string]any{})
	f.stale = &tracker.Relabelled{Now: "Unmute", Was: "Mute"}

	got := a.executeTool(ctx, "click", map[string]any{"n": 1.0})
	if strings.HasPrefix(got, "error") {
		t.Fatalf("the second press was refused: %q", got)
	}
	if len(f.clicked) != 1 {
		t.Fatalf("the button was pressed %d times, want 1", len(f.clicked))
	}
	if !strings.Contains(got, "Unmute") {
		t.Errorf("result = %q, want it to name the button as it is now", got)
	}

	// The other half of the same rule: a name is what the stop line is judged on, so anything that is not a plain rename is still refused rather than pressed.
	f.stale = errors.New("it is now a invalid, not a push button")
	if got := a.executeTool(ctx, "click", map[string]any{"n": 1.0}); !strings.HasPrefix(got, "error") {
		t.Errorf("an element that is no longer a button was pressed anyway: %q", got)
	}
	if len(f.clicked) != 1 {
		t.Errorf("the button was pressed %d times, want the second attempt refused", len(f.clicked))
	}
}

// gemini-3.8-live documents Google Search grounding as supported, but nobody has dialled it with Ora's own handshake, and the failure it would inherit is the 2026-09-02 one: the session closes with "You exceeded your current quota" a quarter second after connecting, before a word is spoken. Grounding stays off until someone probes it, because a missing web search only degrades the session while a 429 ends it.
func TestLiveToolsFor_NoSearchOnLive38(t *testing.T) {
	for _, tool := range liveToolsFor("gemini-3.8-live") {
		if tool.GoogleSearch != nil {
			t.Fatal("3.8 must not be handed Google Search grounding until it is probed")
		}
	}
}
