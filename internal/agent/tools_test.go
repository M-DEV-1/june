package agent

import (
	"context"
	"errors"
	"fmt"
	"june/internal/act"
	"june/internal/db"
	"june/internal/db/dbtest"
	"june/internal/memory"
	"june/internal/tracker"
	"june/internal/window"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
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

// --- save_note ---
//
// Nothing said IN CONVERSATION reached long-term memory before this tool existed: LogNote was only ever called from a /note slash command or the background screen-activity compiler, never from the live agent itself. save_note closes that gap.

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
		"ref": "thread#19", "content": "mf x mdev is a Google Meet call, unrelated to the June work",
	})
	if brain.updatedThreadID != 19 || brain.updatedThreadState != "mf x mdev is a Google Meet call, unrelated to the June work" {
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
		{CreatedAt: base.Add(8 * time.Hour).UTC(), Content: `{"task_name":"June Memory Architecture Development","summary":"recall surgery"}`},
		{CreatedAt: base.Add(9 * time.Hour).UTC(), Content: `{"task_name":"Raw Activity Log","summary":"Unknown | Unknown"}`},
	}
	a := NewAgent(nil, nil, brain, nil, "FAKE_API_KEY")
	out := a.ExecuteTool(context.Background(), "recall", map[string]any{"since": "2026-08-28", "until": "2026-08-28"})
	if !strings.Contains(out, "Brightpath Statement Builder VRDS") || !strings.Contains(out, "June Memory Architecture Development") {
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
		return false, errors.New("the june extension is not loaded in this shell")
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

// TestExecuteTool_ApprovalGatedTools_RefuseInsteadOfBlocking checks that read_file refuses a credential path rather than reading it, since nothing in June can ask the user to approve the read. The path is built with the platform's own separator, so on Windows it also checks that a backslash path meets the patterns.
func TestExecuteTool_ApprovalGatedTools_RefuseInsteadOfBlocking(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	sensitive := filepath.Join(t.TempDir(), ".aws", "config")
	if err := os.MkdirAll(filepath.Dir(sensitive), 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(sensitive, []byte("-----BEGIN PRIVATE KEY-----"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got := a.executeTool(context.Background(), "read_file", map[string]any{"path": sensitive})
	if !strings.HasPrefix(got, "error") || strings.Contains(got, "PRIVATE KEY") {
		t.Errorf("read_file = %q, want a refusal", got)
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

// A Windows desk lists its applications through Get-StartApps, Store apps included, and starts one by its AppID through the shell's AppsFolder. The listing is PowerShell's JSON, which can open with a byte order mark; Calculator must be found by its display name and started from its AppsFolder entry.
func TestExecuteTool_OpenApp_StartsAStoreAppFromItsStartAppsEntry(t *testing.T) {
	a, _ := switchingAgent(t, func() (string, string) { return "explorer", "Desktop" })
	out := "\xef\xbb\xbf" + `[{"Name":"Calculator","AppID":"Microsoft.WindowsCalculator_8wekyb3d8bbwe!App"},{"Name":"Brave","AppID":"Brave"}]`
	a.desktopEntries = func() map[string]string { return startAppEntries([]byte(out)) }
	var launched []string
	a.launchApp = func(entry string) error {
		launched = append(launched, entry)
		return nil
	}
	a.executeTool(WithQuestion(context.Background(), "open calculator"), "open_app", map[string]any{"app": "calculator"})
	if want := `shell:AppsFolder\Microsoft.WindowsCalculator_8wekyb3d8bbwe!App`; len(launched) != 1 || launched[0] != want {
		t.Errorf("launched = %q, want [%s]", launched, want)
	}
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

// The last conversation of 2026-09-12 broke on this three times. June added a research task, the user asked for the right context to be put on it, then asked for it to be deleted, and every attempt was refused: "revise only handles note and thread refs, not \"task\"". The revise tool's own description had been promising action-item support all along, and an action item is a note; a row on the Tasks screen is not, and nothing could touch one.
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

// --- do ---
//
// A chain of several actions said out loud — open Spotify and play this, then open Teams and message someone, then look for new messages — was driven one raw tool call at a time inside the live conversation before this tool existed.
// The job runner in internal/actjob plans first, checks each step against the change it expected, budgets itself at twice its own estimate and reads what this machine did the last few times it was asked something similar, but it was reachable only through POST /act, which only the window posts to.
// So a five-part request spoken aloud got no plan, no verification and no memory of the last run: in the session of 2026-09-12 21:19 a request to read one web page took three and a half minutes and the user brought the window forward himself.
// do is the voice session's way into that runner.

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
