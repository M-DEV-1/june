package db

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// store to hold db conn
type Store struct {
	db              *sql.DB
	mu              sync.RWMutex
	currentParentID int64 // bookmark for session
	currentTaskID   int64 // bookmark for task
	// currentTaskName is the content of the node currentTaskID points at, so LogSemanticNode can tell "the same thread as the last write" from "a different thread that happened to be written next". Without it SameTask files a summary under whichever task was touched last.
	currentTaskName string
	// userID is the root node every day node hangs off, kept so a day can be created after New has returned.
	userID int64
	// daySessions maps a local calendar day ("2006-01-02") to the session node under that day, so LogSemanticNode resolves the day per write instead of pinning the one the process started on. Guarded by mu.
	daySessions map[string]int64
	// clock is the source of "now" for day resolution, overridable by SetClock so a test can write across a midnight boundary. Nil means time.Now.
	clock func() time.Time

	// embedder/vectorIndex back HybridSearch's semantic half (see hybrid.go). Both nilable, wired via SetEmbedder/SetVectorIndex — a Store with neither set runs lexical-only.
	embedder    embedder
	vectorIndex vectorIndex
	// embedsAreFree is set by SetEmbedsAreFree when the embedder is the local engine rather than a metered API. See reconcileBackfillCandidates.
	embedsAreFree bool
	// vectorSimilarityFloor overrides minVectorSimilarity for embedders whose cosine scale differs from Gemini's. Zero means use the default. See SetVectorSimilarityFloor.
	vectorSimilarityFloor float32
	// actRunSimilarityFloor overrides DefaultActRunSimilarity, the cosine a past screen run's question must reach to be offered as reference. Zero means use the default. See SetActRunSimilarityFloor.
	actRunSimilarityFloor float64
	// actRunBackfilling is held for the length of one background pass that embeds act run questions written before they were embedded, so several asks in a row start one pass between them rather than one each. See backfillActRunVectors.
	actRunBackfilling atomic.Bool

	// framesDir is ora-db/frames next to the sqlite file. Empty for :memory: stores — vision JPEGs are skipped.
	framesDir string

	// path is the sqlite file this store was opened from, "" for :memory:. QueryStore uses it to open a second, read-only connection — see query_store.go.
	path string
	// roDB/roOnce/roErr back that second connection, opened lazily on the first QueryStore call and reused after.
	roOnce sync.Once
	roDB   *sql.DB
	roErr  error

	// routineMu guards routineRunning, the in-flight set TryStart/Finish use so the scheduler tick and a POST /routines/{id}/run landing on the same routine at once don't both ask and both write its result. See routines.go.
	routineMu      sync.Mutex
	routineRunning map[int64]bool
}

// constructor, return pointer to struct and err
func New(path string) (*Store, error) {
	// WAL lets multiple connections read/write concurrently (daemon LogEpisode + tool HybridSearch); busy_timeout makes them wait instead of erroring SQLITE_BUSY immediately.
	// foreign_keys is off by default in SQLite and is a property of a connection, not of the database, so it has to be in the DSN: the driver replays every _pragma here on each connection the pool opens, which a one-off Exec after sql.Open would not. Without it every ON DELETE CASCADE below is dead text — deleting a conversation left its turns behind, deleting an episode or a thread left dangling rows in episode_threads, and a turn could be written against a conversation id that names nothing. See foreign_keys_test.go.
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"

	if path != ":memory:" {
		dir := filepath.Dir(path)

		// 0755: owner rwx, group/other rx
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", dsn)

	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite: %w", err)
	}

	// Every connection to ":memory:" opens its own private, empty database, so a pool of them is a pool of unrelated stores: the second connection the pool opens under any concurrency sees no tables at all. Pinning the pool to one connection makes an in-memory store behave like the single database a caller expects. File-backed stores keep the full pool, which is what WAL is for.
	if path == ":memory:" {
		db.SetMaxOpenConns(1)
	}

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping sqlite: %w", err)
	}

	if path != ":memory:" {
		securePermissions(filepath.Dir(path), path)
	}

	s := &Store{db: db, path: path}
	if path != ":memory:" {
		s.framesDir = filepath.Join(filepath.Dir(path), "frames")
	}
	if err := s.createSchema(); err != nil {
		return nil, err
	}

	// USER --> DAY --> SESSION --> ACTIVITY
	// check for user node
	userID, err := s.ensureNode(context.Background(), 0, "user", "default_user") // make this configurable to system user
	if err != nil {
		return nil, fmt.Errorf("failed to ensure user: %w", err)
	}

	// check for day node
	today := time.Now().Format("2006-01-02") // YYYY-MM-DD
	dayID, err := s.ensureNode(context.Background(), userID, "day", today)
	if err != nil {
		return nil, fmt.Errorf("failed to ensure day: %w", err)
	}

	// check for session node
	sessionID, err := s.ensureNode(context.Background(), dayID, "session", "Active Session")
	// TODO: semantic session naming
	if err != nil {
		return nil, fmt.Errorf("failed to ensure session: %w", err)
	}

	s.mu.Lock()
	s.currentParentID = sessionID // bookmark
	s.userID = userID
	s.daySessions = map[string]int64{today: sessionID}

	// rehydrate the latest task ID for continuity
	var taskID int64
	var taskName string
	err = db.QueryRow("SELECT id, content FROM nodes WHERE parent_id = ? AND type = 'task' ORDER BY id DESC LIMIT 1", sessionID).Scan(&taskID, &taskName)
	if err == nil {
		s.currentTaskID = taskID
		s.currentTaskName = taskName
	}
	s.mu.Unlock()

	return s, nil
}

// securePermissions restricts the db directory to 0700 and the main db file plus its WAL/SHM sidecars to 0600 — otherwise the user's entire captured memory defaults to whatever umask created it (commonly 0755/0644, world-readable on a multi-user machine). Best-effort: a chmod failure is logged, not fatal — the app should still start. No-op on Windows, where these POSIX bits don't apply. The WAL/SHM sidecars may not exist yet (SQLite creates them lazily on first write), which is expected and not logged.
func securePermissions(dir, dbPath string) {
	if runtime.GOOS == "windows" {
		return
	}
	if err := os.Chmod(dir, 0700); err != nil {
		slog.Warn("failed to restrict db directory permissions", "dir", dir, "error", err)
	}
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if err := os.Chmod(p, 0600); err != nil && !os.IsNotExist(err) {
			slog.Warn("failed to restrict db file permissions", "path", p, "error", err)
		}
	}
}

// SetClock replaces the source of "now" used to decide which calendar day a summary is filed under. Input: a function returning the current time. Output: none. Only tests call this; production leaves it nil and gets time.Now.
func (s *Store) SetClock(fn func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clock = fn
}

// now returns the current time from the installed clock, or time.Now when none is installed. Caller must hold s.mu (either mode).
func (s *Store) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}

// sessionForDay returns the session node id for the calendar day containing at, creating that day node and its session the first time anything is written to the day. Input: ctx and the instant the write is filed under. Output: the session node's id, or an error from the node writes. Caller must hold s.mu for writing.
func (s *Store) sessionForDay(ctx context.Context, at time.Time) (int64, error) {
	day := DayStart(at).Format("2006-01-02")
	if id, ok := s.daySessions[day]; ok {
		return id, nil
	}
	dayID, err := s.ensureNode(ctx, s.userID, "day", day)
	if err != nil {
		return 0, fmt.Errorf("failed to ensure day: %w", err)
	}
	sessionID, err := s.ensureNode(ctx, dayID, "session", "Active Session")
	if err != nil {
		return 0, fmt.Errorf("failed to ensure session: %w", err)
	}
	if s.daySessions == nil {
		s.daySessions = map[string]int64{}
	}
	s.daySessions[day] = sessionID
	return sessionID, nil
}

func (s *Store) createSchema() error {
	query := `
	CREATE TABLE IF NOT EXISTS nodes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		parent_id INTEGER REFERENCES nodes(id),
		type TEXT NOT NULL,
		content TEXT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		domain TEXT NOT NULL DEFAULT ''
	);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_nodes_unique ON nodes(IFNULL(parent_id, 0), type, content);
	CREATE INDEX IF NOT EXISTS idx_parent_id ON nodes(parent_id);

	-- notes: explicit user-stated facts. always-on, small, forever.
	-- separate from nodes/tree because they're not temporal events.
	CREATE TABLE IF NOT EXISTS notes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		content TEXT NOT NULL,
		kind TEXT NOT NULL DEFAULT 'fact',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_notes_unique ON notes(content, kind);

	-- notes_archive: facts that consolidation merged away. The merge is a model's summary; these rows are the source it summarised, kept so a wrong merge can be traced and undone. Never searched, never shown to the consolidator.
	CREATE TABLE IF NOT EXISTS notes_archive (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		note_id INTEGER NOT NULL,
		content TEXT NOT NULL,
		kind TEXT NOT NULL,
		created_at DATETIME,
		updated_at DATETIME,
		archived_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	-- personal_context: the small set of things known for certain about the
	-- user -- who they are, the people in their life, preferences they stated.
	-- Keyed by subject and edited in place, never appended to, and only ever
	-- written from something the user said themselves. No FTS, no vectors: it
	-- is injected whole into every prompt rather than retrieved, so there is
	-- nothing to rank and nothing to miss. Its own table so that no compaction
	-- or consolidation path can reach it.
	CREATE TABLE IF NOT EXISTS personal_context (
		id INTEGER PRIMARY KEY,
		subject TEXT UNIQUE NOT NULL,
		content TEXT NOT NULL,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	-- folds: a branch() subtask's result that couldn't be delivered into the
	-- live session that requested it. Staged here to surface at the next
	-- session's handshake instead of being silently dropped. Deliberately
	-- separate from notes -- a fold is a one-off task result, not a durable
	-- user fact, and belongs nowhere near ReconcileNotes' identity-fact
	-- reconciliation pass.
	CREATE TABLE IF NOT EXISTS folds (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		task TEXT NOT NULL,
		result TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		consumed_at DATETIME
	);

	-- single-row cache: synthesized "what is the user doing right now" summary.
	-- recomputed on a cadence by the daemon, disposable, replaced in full each time.
	CREATE TABLE IF NOT EXISTS working_state (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		content TEXT NOT NULL,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	-- FTS5 over summary content + note content.
	-- triggers below keep it in sync. tokenizer 'unicode61' is FTS5 default.
	CREATE VIRTUAL TABLE IF NOT EXISTS memory_fts USING fts5(
		content,
		source UNINDEXED,
		ref_id UNINDEXED,
		tokenize = 'unicode61'
	);

	-- Drop the old trigger so existing dev DBs pick up the updated WHEN clause and the $.summary extraction below.
	DROP TRIGGER IF EXISTS nodes_ai_summary;
	-- A summary node's content is the whole marshalled TaskSummary (see LogSemanticNode), so indexing it verbatim made its JSON keys ('same_task', 'task_name') live search terms that matched every summary ever written. Index the summary prose instead. Digest nodes store plain prose and fall through unchanged, same expression SearchMemory uses on the read path.
	CREATE TRIGGER IF NOT EXISTS nodes_ai_summary AFTER INSERT ON nodes
	WHEN NEW.type IN ('summary','digest')
	BEGIN
		INSERT INTO memory_fts(content, source, ref_id) VALUES (
			CASE WHEN json_valid(NEW.content)
				THEN IFNULL(NULLIF(json_extract(NEW.content, '$.summary'), ''), NEW.content)
				ELSE NEW.content
			END, NEW.type, NEW.id);
	END;

	CREATE TRIGGER IF NOT EXISTS nodes_ad_summary AFTER DELETE ON nodes
	WHEN OLD.type IN ('summary','digest')
	BEGIN
		DELETE FROM memory_fts WHERE source IN ('summary','digest') AND ref_id = OLD.id;
	END;

	-- A digest is rewritten in place when a day is compacted a second time (see ReplaceSummariesWithDigest), and without this the FTS row would still hold the first digest's words. Scoped to UPDATE OF content so the reparent update, which only touches parent_id, does not churn the index.
	CREATE TRIGGER IF NOT EXISTS nodes_au_summary AFTER UPDATE OF content ON nodes
	WHEN NEW.type IN ('summary','digest')
	BEGIN
		DELETE FROM memory_fts WHERE source IN ('summary','digest') AND ref_id = OLD.id;
		INSERT INTO memory_fts(content, source, ref_id) VALUES (
			CASE WHEN json_valid(NEW.content)
				THEN IFNULL(NULLIF(json_extract(NEW.content, '$.summary'), ''), NEW.content)
				ELSE NEW.content
			END, NEW.type, NEW.id);
	END;

	CREATE TRIGGER IF NOT EXISTS notes_ai AFTER INSERT ON notes
	BEGIN
		INSERT INTO memory_fts(content, source, ref_id) VALUES (NEW.content, 'note', NEW.id);
	END;

	CREATE TRIGGER IF NOT EXISTS notes_ad AFTER DELETE ON notes
	BEGIN
		DELETE FROM memory_fts WHERE source = 'note' AND ref_id = OLD.id;
	END;

	CREATE TRIGGER IF NOT EXISTS notes_au AFTER UPDATE ON notes
	BEGIN
		DELETE FROM memory_fts WHERE source = 'note' AND ref_id = OLD.id;
		INSERT INTO memory_fts(content, source, ref_id) VALUES (NEW.content, 'note', NEW.id);
	END;

	-- threads: ongoing throughlines in the user's life (a show, a project, a
	-- person), each with a current state = where the user is *within* it.
	-- concurrent threads coexist; they are never collapsed into one another.
	CREATE TABLE IF NOT EXISTS threads (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		subject TEXT NOT NULL,
		kind TEXT NOT NULL DEFAULT 'work',
		state TEXT,
		salience REAL NOT NULL DEFAULT 0.5,
		times_seen INTEGER NOT NULL DEFAULT 1,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		last_seen_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		status TEXT NOT NULL DEFAULT 'active'
	);
	-- episode_threads: which captures belong to which ongoing piece of work.
	-- The compiler already decides this on every flush — it hands a buffer of
	-- activities to the model and gets back the threads they belong to — and
	-- until this table existed that decision was thrown away each time. The
	-- result was a store holding thousands of episodes and hundreds of threads
	-- with nothing joining them: a thread could say "reviewed the code, eleven
	-- findings" and no query could reach the screens the findings were on.
	-- ON DELETE CASCADE both ways: an edge to a thread or episode that no
	-- longer exists is not a fact about anything.
	CREATE TABLE IF NOT EXISTS episode_threads (
		episode_id INTEGER NOT NULL REFERENCES episodes(id) ON DELETE CASCADE,
		thread_id  INTEGER NOT NULL REFERENCES threads(id)  ON DELETE CASCADE,
		PRIMARY KEY (episode_id, thread_id)
	);
	CREATE INDEX IF NOT EXISTS idx_episode_threads_thread ON episode_threads(thread_id);

	CREATE UNIQUE INDEX IF NOT EXISTS idx_threads_subject ON threads(subject, kind);
	CREATE INDEX IF NOT EXISTS idx_threads_last_seen ON threads(last_seen_at);

	CREATE TRIGGER IF NOT EXISTS threads_ai AFTER INSERT ON threads BEGIN
		INSERT INTO memory_fts(content, source, ref_id) VALUES (NEW.subject || ' — ' || IFNULL(NEW.state,''), 'thread', NEW.id);
	END;
	CREATE TRIGGER IF NOT EXISTS threads_ad AFTER DELETE ON threads BEGIN
		DELETE FROM memory_fts WHERE source='thread' AND ref_id = OLD.id;
	END;
	CREATE TRIGGER IF NOT EXISTS threads_au AFTER UPDATE ON threads BEGIN
		DELETE FROM memory_fts WHERE source='thread' AND ref_id = OLD.id;
		INSERT INTO memory_fts(content, source, ref_id) VALUES (NEW.subject || ' — ' || IFNULL(NEW.state,''), 'thread', NEW.id);
	END;

	-- episodes: append-only, NON-deduped time series of dwell-confirmed
	-- captures. Deliberately NOT part of the nodes tree: nodes' unique index
	-- on (parent_id,type,content) would collapse repeat visits to the same
	-- app|title into one row, which is exactly wrong here — the whole point
	-- is to keep every visit, including its (possibly different) screen_text.
	CREATE TABLE IF NOT EXISTS episodes (
		id INTEGER PRIMARY KEY,
		created_at DATETIME NOT NULL DEFAULT (datetime('now')),
		app TEXT NOT NULL,
		title TEXT NOT NULL,
		screen_text TEXT NOT NULL DEFAULT '',
		importance REAL NOT NULL DEFAULT 0,
		domain TEXT NOT NULL DEFAULT '',
		user_activity TEXT NOT NULL DEFAULT '',
		visible_text TEXT NOT NULL DEFAULT '',
		image_path TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX IF NOT EXISTS idx_episodes_created_at ON episodes(created_at);
	CREATE INDEX IF NOT EXISTS idx_episodes_app_title ON episodes(app, title);

	-- separate FTS5 index (not memory_fts) so raw screen captures don't dilute
	-- summary/note/thread relevance ranking; SearchEpisodes queries it directly.
	CREATE VIRTUAL TABLE IF NOT EXISTS episodes_fts USING fts5(
		screen_text,
		content = 'episodes',
		content_rowid = 'id',
		tokenize = 'unicode61'
	);

	CREATE TRIGGER IF NOT EXISTS episodes_ai AFTER INSERT ON episodes BEGIN
		INSERT INTO episodes_fts(rowid, screen_text) VALUES (NEW.id, NEW.screen_text);
	END;

	-- episodes_fts is an EXTERNAL CONTENT fts5 table (content='episodes'): it
	-- has no content of its own, only the index. Removing/updating an index
	-- entry therefore requires the special 'delete' command with the OLD
	-- column values passed explicitly — a plain DELETE FROM episodes_fts
	-- WHERE rowid=? silently fails to update the postings list, leaving
	-- stale content searchable. See https://sqlite.org/fts5.html#the_delete_command.
	CREATE TRIGGER IF NOT EXISTS episodes_ad AFTER DELETE ON episodes BEGIN
		INSERT INTO episodes_fts(episodes_fts, rowid, screen_text) VALUES ('delete', OLD.id, OLD.screen_text);
	END;

	-- An update that rewrites or clears screen_text must not leave the old text searchable; this
	-- trigger keeps the FTS mirror from continuing to surface the cleared text.
	CREATE TRIGGER IF NOT EXISTS episodes_au AFTER UPDATE ON episodes BEGIN
		INSERT INTO episodes_fts(episodes_fts, rowid, screen_text) VALUES ('delete', OLD.id, OLD.screen_text);
		INSERT INTO episodes_fts(rowid, screen_text) VALUES (NEW.id, NEW.screen_text);
	END;

	-- diary: Ora's own first-person record. kind 'day' holds one entry per local
	-- calendar day (day = 'YYYY-MM-DD'); kind 'understanding' is the single bounded
	-- current-model-of-the-user document (day = ''), rewritten in place each evening;
	-- kind 'brief' records the morning brief delivered that day and doubles as its
	-- once-per-day marker; kind 'dream' is a night's morning report. The dreaming
	-- loop's compaction collapses old 'day' rows into 'week' (day = the Monday) and
	-- old 'week' rows into 'month' (day = 'YYYY-MM-01'); a collapsed row is kept and
	-- gets parent_id set to the coarse row that now summarises it, the way
	-- ReplaceSummariesWithDigest reparents summaries. Upserted by (day, kind) —
	-- see SetDiaryEntry.
	CREATE TABLE IF NOT EXISTS diary (
		id INTEGER PRIMARY KEY,
		day TEXT NOT NULL,
		kind TEXT NOT NULL DEFAULT 'day',
		content TEXT NOT NULL,
		parent_id INTEGER,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(day, kind)
	);

	-- Mirrored into memory_fts exactly like threads, so diary entries surface through the existing query_memory path with no agent changes.
	-- Every kind is mirrored but TaskNoticeWatermarkKind, which is a bare note id the proactive loop rewrites on most ticks rather than anything Ora wrote (see diary.go).
	-- The insert is written as INSERT ... SELECT ... WHERE rather than a trigger-level WHEN so the update trigger's DELETE still runs for every kind, which is what clears a watermark row an older database had already mirrored.
	-- Dropped first so a database created before the watermark was excluded picks up the new bodies.
	DROP TRIGGER IF EXISTS diary_ai;
	DROP TRIGGER IF EXISTS diary_au;
	CREATE TRIGGER IF NOT EXISTS diary_ai AFTER INSERT ON diary BEGIN
		INSERT INTO memory_fts(content, source, ref_id)
			SELECT NEW.content, 'diary', NEW.id WHERE NEW.kind <> '` + TaskNoticeWatermarkKind + `';
	END;
	CREATE TRIGGER IF NOT EXISTS diary_ad AFTER DELETE ON diary BEGIN
		DELETE FROM memory_fts WHERE source='diary' AND ref_id = OLD.id;
	END;
	CREATE TRIGGER IF NOT EXISTS diary_au AFTER UPDATE ON diary BEGIN
		DELETE FROM memory_fts WHERE source='diary' AND ref_id = OLD.id;
		INSERT INTO memory_fts(content, source, ref_id)
			SELECT NEW.content, 'diary', NEW.id WHERE NEW.kind <> '` + TaskNoticeWatermarkKind + `';
	END;

	-- dream_runs: one row per night of the overnight dreaming loop, keyed by the
	-- night's local date. The PRIMARY KEY is the single-run-per-night guarantee;
	-- stages_done lets an interrupted night resume only what is missing.
	CREATE TABLE IF NOT EXISTS dream_runs (
		night TEXT PRIMARY KEY,
		started_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		finished_at DATETIME,
		stages_done TEXT NOT NULL DEFAULT '',
		grinder TEXT NOT NULL DEFAULT '',
		report TEXT NOT NULL DEFAULT ''
	);

	-- hypotheses: the dreaming loop's private guesses about the user, tracked
	-- across nights until promoted or retired. Deliberately NO FTS triggers on
	-- this table or dream_runs: hypotheses are unvetted working state and must
	-- never surface through retrieval into a prompt — only the finished dream
	-- report enters the (indexed) diary.
	CREATE TABLE IF NOT EXISTS hypotheses (
		id INTEGER PRIMARY KEY,
		statement TEXT NOT NULL UNIQUE,
		confidence TEXT NOT NULL DEFAULT 'low',
		status TEXT NOT NULL DEFAULT 'open',
		born TEXT NOT NULL,
		last_tested TEXT,
		times_tested INTEGER NOT NULL DEFAULT 0,
		evidence TEXT NOT NULL DEFAULT '',
		reason TEXT NOT NULL DEFAULT ''
	);

	-- tally: self-accounting counters, one row per local calendar day per provider.
	-- "provider" is either a brain backend name ("claude-cli", "gemini", ...) or one
	-- of two vector-contribution pseudo-providers written by hybrid.go's fusion
	-- counter: "vector-queries" (one row per HybridSearch call) and "vector-hits"
	-- (one row per call where a vector-arm candidate survived into the final top-k).
	-- For real brain providers, total_ms accumulates call latency in milliseconds;
	-- for "vector-hits" that column is repurposed to accumulate the raw count of
	-- surviving vector candidates instead (see recordVectorContribution) rather than
	-- add a second table for one integer. See internal/tally for the reader/writer.
	-- conversations, conversation_turns and user_tasks are the desktop window's own rows: the threads of question and answer the window keeps, every turn inside one, and the tasks the user types in themselves. Deliberately outside notes and nodes — a turn is a record of what was said in the window, not a fact about the user, and nothing in retrieval, consolidation or the dreaming loop should ever see one. Action items stay where they are, as notes of kind 'action'.
	CREATE TABLE IF NOT EXISTS conversations (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		title TEXT NOT NULL DEFAULT '',
		brain TEXT NOT NULL DEFAULT '',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE TABLE IF NOT EXISTS conversation_turns (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		conversation_id INTEGER NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
		role TEXT NOT NULL,
		text TEXT NOT NULL,
		kind TEXT NOT NULL DEFAULT 'ask',
		-- evidence and tools are stored as the JSON the window reads: the supporting rows behind an answer, and the names of the tools the agent called on the way to it.
		evidence TEXT NOT NULL DEFAULT '',
		tools TEXT NOT NULL DEFAULT '',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_conversation_turns_conv ON conversation_turns(conversation_id, id);
	CREATE INDEX IF NOT EXISTS idx_conversation_turns_created ON conversation_turns(created_at);
	CREATE TABLE IF NOT EXISTS user_tasks (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		title TEXT NOT NULL,
		done INTEGER NOT NULL DEFAULT 0,
		conversation_id INTEGER NOT NULL DEFAULT 0,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	-- routines: user-authored scheduled instructions Ora checks on its own — "every weekday at 8, tell me the one thing I must do today", "when Priya replies about the venue, tell me". schedule is left as the free text the user typed; internal/proactive parses it into when to check. last_answer holds what the model said the last time it ran, "NOTHING" included, so the window can show what happened without re-running it. Not memory — never indexed, never searched.
	CREATE TABLE IF NOT EXISTS routines (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		text TEXT NOT NULL,
		schedule TEXT NOT NULL,
		enabled INTEGER NOT NULL DEFAULT 1,
		last_run DATETIME,
		last_answer TEXT NOT NULL DEFAULT '',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	-- snoozes holds the notices the user pushed to later from a notification's own buttons ("In an hour", "This evening", "Tomorrow"). notice_kind and notice_id are the notice's own, carried so a re-fired snooze can still act on what it was about; title and body are the text to post again; fired_at is NULL until the scheduler has posted it, which is what stops one snooze firing twice. Not memory — never indexed, never searched.
	CREATE TABLE IF NOT EXISTS snoozes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		notice_kind TEXT NOT NULL DEFAULT '',
		notice_id TEXT NOT NULL DEFAULT '',
		title TEXT NOT NULL DEFAULT '',
		body TEXT NOT NULL DEFAULT '',
		due_at DATETIME NOT NULL,
		fired_at DATETIME,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_snoozes_due ON snoozes(fired_at, due_at);

	-- act_runs records every ask whose trace called a screen tool (observe_screen, point_at, click, scroll_to, type_text), for the later "watch me once" replay learning. steps_json is the JSON array of ActStep: each tool hop's name, args and the first 300 runes of its result; type_text's own text argument (whatever the user dictated) is dropped before it ever reaches this column. Not memory itself, and never searched except by SimilarActRuns (act_reference.go) — but a run's rendered steps do reach a model, either folded into a nightly "How I did X" note (internal/dream/procedures.go) or straight into a new screen ask's prompt (internal/agent/act_reference.go); see act_runs.go's header for the full path.
	CREATE TABLE IF NOT EXISTS act_runs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		started_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		question TEXT NOT NULL,
		model TEXT NOT NULL DEFAULT '',
		outcome TEXT NOT NULL,
		answer TEXT NOT NULL DEFAULT '',
		error TEXT NOT NULL DEFAULT '',
		duration_ms INTEGER NOT NULL DEFAULT 0,
		steps_json TEXT NOT NULL DEFAULT '[]',
		job_id TEXT NOT NULL DEFAULT '',
		job_state TEXT NOT NULL DEFAULT '',
		job_json TEXT NOT NULL DEFAULT ''
	);

	-- act_run_vectors holds the embedding of each act run's question, as little-endian float32s (see encodeActRunVector), written when the run is stored and read only by SimilarActRuns to score how close a new question is to that one.
	-- Its own table rather than a column on act_runs for two reasons. A blob column would ride along in every "SELECT * FROM act_runs" the query tool writes, putting kilobytes of binary into a prompt. And these vectors are deliberately not in the chromem index that backs memory search: HybridSearch searches that index whole, so an act run put in it would come back as a memory hit, which act_runs.go's header says must never happen.
	-- The row dies with its run: foreign keys are on for every connection (see the DSN), so PruneActRuns deleting a run takes its vector with it.
	CREATE TABLE IF NOT EXISTS act_run_vectors (
		run_id INTEGER PRIMARY KEY REFERENCES act_runs(id) ON DELETE CASCADE,
		vec BLOB NOT NULL
	);

	-- token_use records one row per model call so the user can see what each provider is costing them. Separate from tally, which holds one counter row per day per provider and so can answer neither "which model" nor "which call": this table keeps the call itself, with the model slug the call actually reached, the channel it came through, the tokens each side of it, and the first 200 runes of the question so a row can be recognised in a log view. Not memory — never indexed, never searched, never fed to a model.
	CREATE TABLE IF NOT EXISTS token_use (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		provider TEXT NOT NULL,
		model TEXT NOT NULL DEFAULT '',
		channel TEXT NOT NULL DEFAULT '',
		input_tokens INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0,
		total_tokens INTEGER NOT NULL DEFAULT 0,
		-- cached_tokens is how much of input_tokens the provider answered out of its own prompt cache rather than reading afresh; part of input_tokens, not extra to it. rounds is how many model calls the question took, since the counts above are already summed over them.
		cached_tokens INTEGER NOT NULL DEFAULT 0,
		rounds INTEGER NOT NULL DEFAULT 0,
		duration_ms INTEGER NOT NULL DEFAULT 0,
		question TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);
	-- Every read of this table is a time window — since an instant, or the last n days — grouped by provider, so the index leads with the timestamp the window scans and carries the provider along with it.
	CREATE INDEX IF NOT EXISTS idx_token_use_created_at ON token_use(created_at, provider);

	CREATE TABLE IF NOT EXISTS tally (
		day TEXT NOT NULL,
		provider TEXT NOT NULL,
		calls INTEGER NOT NULL DEFAULT 0,
		failures INTEGER NOT NULL DEFAULT 0,
		total_ms INTEGER NOT NULL DEFAULT 0,
		-- Characters in and out, not tokens: the Brain seam carries no usage metadata and the CLI providers report none, so characters are what every provider can actually be measured in. A token estimate is a read-time division, kept out of the stored data.
		prompt_chars INTEGER NOT NULL DEFAULT 0,
		reply_chars INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (day, provider)
	);
	`
	// db struc: USER --> DAY --> SESSION --> ACTIVITY
	// TODO: salience score to prioritize important activities and not track menial activities
	// i.e. what do we choose to remember
	if _, err := s.db.Exec(query); err != nil {
		return err
	}

	// Migration for pre-existing DBs from before the domain column existed. modernc.org/sqlite doesn't support ALTER TABLE ADD COLUMN IF NOT EXISTS (confirmed empirically — syntax error, not a no-op), so ensureColumn checks via PRAGMA table_info instead. Safe to run on every startup, including brand-new DBs where CREATE TABLE already added the column.
	if err := s.ensureColumn("nodes", "domain", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn("episodes", "domain", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn("episodes", "user_activity", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn("episodes", "visible_text", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn("episodes", "image_path", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	// tally gained its character counts on 2026-09-01 in the create statement only, so every existing database kept failing each bump with "no column named prompt_chars".
	if err := s.ensureColumn("tally", "prompt_chars", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn("tally", "reply_chars", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	// token_use gained cached_tokens and rounds on 2026-09-05 in the create statement only, so an existing database kept the old shape.
	if err := s.ensureColumn("token_use", "cached_tokens", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn("token_use", "rounds", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	// act_runs gained the three job columns on 2026-09-05 for the computer-use job checkpoints (act_jobs.go), again in the create statement only.
	for _, col := range []struct{ name, decl string }{{"job_id", "TEXT NOT NULL DEFAULT ''"}, {"job_state", "TEXT NOT NULL DEFAULT ''"}, {"job_json", "TEXT NOT NULL DEFAULT ''"}} {
		if err := s.ensureColumn("act_runs", col.name, col.decl); err != nil {
			return err
		}
	}
	// diary gained parent_id on 2026-09-06, when compaction stopped deleting the day pages it rolls into a week and started reparenting them under it instead.
	if err := s.ensureColumn("diary", "parent_id", "INTEGER"); err != nil {
		return err
	}
	// notes gained owner_class on 2026-09-05 so the user can correct an action item's "me"/"them"/"unclear" reading by hand; empty for every note that is not an action item and for one nobody has corrected yet.
	if err := s.ensureColumn("notes", "owner_class", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}

	// Migration for DBs written before nodes_ai_summary extracted $.summary: their summary rows still hold the raw marshalled TaskSummary, so the JSON keys stay searchable until the text is rewritten. Idempotent — a rewritten row is no longer JSON, so the guard skips it on every later run.
	if _, err := s.db.Exec(`
		UPDATE memory_fts SET content = json_extract(content, '$.summary')
		WHERE source IN ('summary','digest')
			AND json_valid(content)
			AND NULLIF(json_extract(content, '$.summary'), '') IS NOT NULL`); err != nil {
		return fmt.Errorf("rebuild summary fts content: %w", err)
	}

	// Migration for DBs written while the diary triggers still mirrored every kind: the task-notice watermark's bare note id is sitting in the search index as if it were a memory. Idempotent — there is nothing left to delete on every later run.
	if _, err := s.db.Exec(`
		DELETE FROM memory_fts
		WHERE source = 'diary'
		  AND ref_id IN (SELECT id FROM diary WHERE kind = ?)`, TaskNoticeWatermarkKind); err != nil {
		return fmt.Errorf("clear the task notice watermark from the search index: %w", err)
	}

	return nil
}

// ensureColumn adds column to table (with the given SQL type/constraint) if it doesn't already exist, checked via PRAGMA table_info since modernc.org/sqlite doesn't support ALTER TABLE ADD COLUMN IF NOT EXISTS.
func (s *Store) ensureColumn(table, column, coldef string) error {
	rows, err := s.db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return fmt.Errorf("ensure column %s.%s: pragma table_info: %w", table, column, err)
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return fmt.Errorf("ensure column %s.%s: scan pragma row: %w", table, column, err)
		}
		if name == column {
			return nil // already present
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("ensure column %s.%s: iterate pragma rows: %w", table, column, err)
	}

	if _, err := s.db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, coldef)); err != nil {
		return fmt.Errorf("ensure column %s.%s: alter table: %w", table, column, err)
	}
	return nil
}

func (s *Store) Close() error {
	if s.roDB != nil {
		s.roDB.Close()
	}
	return s.db.Close()
}

// DB exposes the underlying connection for test-only raw queries.
func (s *Store) DB() *sql.DB { return s.db }

// LatestMemoryTime is the newest timestamp across episodes, threads, and working_state. Input: ctx. Output: the store's own "now" for recency, or zero if the store is empty. Evals use this instead of wall-clock so a snapshot scored the next day is not compared against the wrong today.
func (s *Store) LatestMemoryTime(ctx context.Context) (time.Time, error) {
	var raw sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT MAX(t) FROM (
			SELECT MAX(created_at) AS t FROM episodes
			UNION ALL
			SELECT MAX(last_seen_at) AS t FROM threads
			UNION ALL
			SELECT MAX(updated_at) AS t FROM working_state
		)`).Scan(&raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("latest memory time: %w", err)
	}
	if !raw.Valid {
		return time.Time{}, nil
	}
	return parseSQLiteTime(raw.String), nil
}

// MemoryAsOf returns when a named source last happened. Input: source like "note:103", "thread:24", "episode:12", "episode:recent", or "working_state". Output: that row's created/last-seen/updated time, or zero if the source is missing or unknown.
func (s *Store) MemoryAsOf(ctx context.Context, source string) (time.Time, error) {
	kind, id, ok := splitMemorySource(source)
	if !ok {
		return time.Time{}, nil
	}
	var raw sql.NullString
	var err error
	switch kind {
	case "working_state":
		err = s.db.QueryRowContext(ctx, `SELECT updated_at FROM working_state WHERE id = 1`).Scan(&raw)
	case "note":
		err = s.db.QueryRowContext(ctx, `SELECT created_at FROM notes WHERE id = ?`, id).Scan(&raw)
	case "thread":
		err = s.db.QueryRowContext(ctx, `SELECT last_seen_at FROM threads WHERE id = ?`, id).Scan(&raw)
	case "episode":
		err = s.db.QueryRowContext(ctx, `SELECT created_at FROM episodes WHERE id = ?`, id).Scan(&raw)
	case "episode_recent":
		err = s.db.QueryRowContext(ctx, `SELECT created_at FROM episodes ORDER BY created_at DESC LIMIT 1`).Scan(&raw)
	default:
		return time.Time{}, nil
	}
	if err == sql.ErrNoRows {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("memory as of %s: %w", source, err)
	}
	if !raw.Valid {
		return time.Time{}, nil
	}
	return parseSQLiteTime(raw.String), nil
}

func splitMemorySource(source string) (kind string, id int64, ok bool) {
	source = strings.TrimSpace(source)
	if source == "" {
		return "", 0, false
	}
	if source == "working_state" {
		return "working_state", 0, true
	}
	if source == "episode:recent" {
		return "episode_recent", 0, true
	}
	kind, rest, found := strings.Cut(source, ":")
	if !found || rest == "" {
		return "", 0, false
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || id <= 0 {
		return "", 0, false
	}
	switch kind {
	case "note", "thread", "episode":
		return kind, id, true
	default:
		return "", 0, false
	}
}
