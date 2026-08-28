package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"ora/internal/memory"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"ora/internal/obs"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	_ "modernc.org/sqlite" // blank import
)

// store to hold db conn
type Store struct {
	db              *sql.DB
	mu              sync.RWMutex
	currentParentID int64 // bookmark for session
	currentTaskID   int64 // bookmark for task

	// embedder/vectorIndex back HybridSearch's semantic half (see hybrid.go). Both nilable, wired via SetEmbedder/SetVectorIndex — a Store with neither set runs lexical-only.
	embedder    embedder
	vectorIndex vectorIndex
	// embedsAreFree is set by SetEmbedsAreFree when the embedder is the local engine rather than a metered API. See reconcileBackfillCandidates.
	embedsAreFree bool
	// vectorSimilarityFloor overrides minVectorSimilarity for embedders whose cosine scale differs from Gemini's. Zero means use the default. See SetVectorSimilarityFloor.
	vectorSimilarityFloor float32

	// framesDir is ora-db/frames next to the sqlite file. Empty for :memory: stores — vision JPEGs are skipped.
	framesDir string
}

// constructor, return pointer to struct and err
func New(path string) (*Store, error) {
	// WAL lets multiple connections read/write concurrently (daemon LogEpisode + tool HybridSearch); busy_timeout makes them wait instead of erroring SQLITE_BUSY immediately.
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"

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

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping sqlite: %w", err)
	}

	if path != ":memory:" {
		securePermissions(filepath.Dir(path), path)
	}

	s := &Store{db: db}
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

	// rehydrate the latest task ID for continuity
	var taskID int64
	err = db.QueryRow("SELECT id FROM nodes WHERE parent_id = ? AND type = 'task' ORDER BY id DESC LIMIT 1", sessionID).Scan(&taskID)
	if err == nil {
		s.currentTaskID = taskID
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

	-- AgeEpisodes (Cycle 5) updates screen_text in place to reclaim space; this
	-- trigger keeps the FTS mirror from continuing to surface the cleared text.
	CREATE TRIGGER IF NOT EXISTS episodes_au AFTER UPDATE ON episodes BEGIN
		INSERT INTO episodes_fts(episodes_fts, rowid, screen_text) VALUES ('delete', OLD.id, OLD.screen_text);
		INSERT INTO episodes_fts(rowid, screen_text) VALUES (NEW.id, NEW.screen_text);
	END;
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

	// Migration for DBs written before nodes_ai_summary extracted $.summary: their summary rows still hold the raw marshalled TaskSummary, so the JSON keys stay searchable until the text is rewritten. Idempotent — a rewritten row is no longer JSON, so the guard skips it on every later run.
	if _, err := s.db.Exec(`
		UPDATE memory_fts SET content = json_extract(content, '$.summary')
		WHERE source IN ('summary','digest')
			AND json_valid(content)
			AND NULLIF(json_extract(content, '$.summary'), '') IS NOT NULL`); err != nil {
		return fmt.Errorf("rebuild summary fts content: %w", err)
	}

	return s.migrateIdentityNote()
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

// Note is a stable, user-stated fact. Always-on in implicit context.
type Note struct {
	ID        int64
	Content   string
	Kind      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// normalizeNoteContent trims, collapses internal whitespace to single spaces, and lowercases — used by LogNote's dedup check to catch paraphrased restatements.
// Does not strip punctuation, so "user likes go" and "user likes go." still stay distinct rows.
func normalizeNoteContent(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// LogNote inserts a note. Idempotent on (content, kind) — returns existing id.
// Also dedupes paraphrased restatements: existing notes of the same kind are compared via normalizeNoteContent, so "User likes Go" and "user likes go" collapse to one row without needing a semantic/embedding index.
// Storage keeps the original casing/whitespace though — the first-ever version of a fact wins and is what every later paraphrase resolves back to (GetNotes/ExistingNotes/RetrieveRelevant all depend on this original casing surviving).
//
// The exact-match (content, kind) unique index still backs the INSERT OR IGNORE path below for byte-identical restatements and is what actually guards concurrent identical inserts — the normalized-comparison scan above is an application-level, non-atomic check and doesn't itself prevent a race between two differently-cased paraphrases.
func (s *Store) LogNote(ctx context.Context, content, kind string) (int64, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.LogNote")
	defer span.End()

	if kind == "" {
		kind = "fact"
	}

	normalized := normalizeNoteContent(content)
	if id, found, err := s.findNoteByNormalizedContent(ctx, normalized, kind); err != nil {
		span.RecordError(err)
		return 0, err
	} else if found {
		span.SetAttributes(attribute.Int64("db.note_id", id))
		return id, nil
	}

	if _, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO notes (content, kind) VALUES (?, ?)`,
		content, kind); err != nil {
		span.RecordError(err)
		return 0, fmt.Errorf("insert note: %w", err)
	}

	var id int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT id FROM notes WHERE content = ? AND kind = ?`,
		content, kind).Scan(&id); err != nil {
		span.RecordError(err)
		return 0, fmt.Errorf("read note id: %w", err)
	}
	span.SetAttributes(attribute.Int64("db.note_id", id))

	// Async, best-effort embedding — same non-blocking pattern as LogEpisode (own context; see its doc comment for why). notes.domain doesn't exist as a column, so metadata just omits the "domain" key rather than sending it empty.
	s.mu.RLock()
	emb, vidx := s.embedder, s.vectorIndex
	s.mu.RUnlock()
	if emb != nil && vidx != nil && strings.TrimSpace(content) != "" {
		go func(id int64, text string) {
			embedCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			vec, err := emb.Embed(embedCtx, "RETRIEVAL_DOCUMENT", text)
			if err != nil {
				slog.Error("async note embed failed", "note_id", id, "error", err)
				return
			}
			meta := map[string]string{
				"source":     "note",
				"kind":       string(memory.KindFact),
				"created_at": time.Now().UTC().Format(time.RFC3339),
			}
			if err := vidx.Add(embedCtx, fmt.Sprintf("note:%d", id), text, vec, meta); err != nil {
				slog.Error("async note vector index add failed", "note_id", id, "error", err)
			}
		}(id, content)
	}
	return id, nil
}

// findNoteByNormalizedContent scans existing notes of kind for one whose content normalizes to the same value as normalized. Used by LogNote to catch paraphrased restatements that the exact-match (content, kind) unique index would not.
func (s *Store) findNoteByNormalizedContent(ctx context.Context, normalized, kind string) (int64, bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, content FROM notes WHERE kind = ?`, kind)
	if err != nil {
		return 0, false, fmt.Errorf("scan existing notes for dedup: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var existingContent string
		if err := rows.Scan(&id, &existingContent); err != nil {
			return 0, false, fmt.Errorf("scan existing note row: %w", err)
		}
		if normalizeNoteContent(existingContent) == normalized {
			return id, true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return 0, false, fmt.Errorf("iterate existing notes: %w", err)
	}
	return 0, false, nil
}

// GetNotes returns all notes ordered newest first.
func (s *Store) GetNotes(ctx context.Context) ([]Note, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.GetNotes")
	defer span.End()

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, content, kind, created_at, updated_at FROM notes ORDER BY id DESC`)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("query notes: %w", err)
	}
	defer rows.Close()

	var out []Note
	for rows.Next() {
		var n Note
		if err := rows.Scan(&n.ID, &n.Content, &n.Kind, &n.CreatedAt, &n.UpdatedAt); err != nil {
			span.RecordError(err)
			return nil, fmt.Errorf("scan note: %w", err)
		}
		out = append(out, n)
	}
	span.SetAttributes(attribute.Int("db.note_count", len(out)))
	return out, nil
}

// SetWorkingState upserts the single-row working-state cache. Content is synthesized by the daemon's state deriver and overwritten in full each cadence tick — no history is kept.
func (s *Store) SetWorkingState(ctx context.Context, content string) error {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.SetWorkingState")
	defer span.End()

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO working_state(id, content, updated_at) VALUES(1, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(id) DO UPDATE SET content = excluded.content, updated_at = CURRENT_TIMESTAMP`,
		content)
	if err != nil {
		span.RecordError(err)
		return fmt.Errorf("set working state: %w", err)
	}
	return nil
}

// GetWorkingState returns the cached working-state content, or ("", nil) when no row has been written yet.
func (s *Store) GetWorkingState(ctx context.Context) (string, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.GetWorkingState")
	defer span.End()

	var content string
	err := s.db.QueryRowContext(ctx, `SELECT content FROM working_state WHERE id = 1`).Scan(&content)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		span.RecordError(err)
		return "", fmt.Errorf("get working state: %w", err)
	}
	return content, nil
}

// RecentSummaries returns the content of the most recent summary and digest nodes, newest-first, up to limit rows. Used by the state deriver to build the synthesis prompt without walking the full ancestor tree.
func (s *Store) RecentSummaries(ctx context.Context, limit int) ([]string, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.RecentSummaries")
	defer span.End()

	rows, err := s.db.QueryContext(ctx,
		`SELECT content FROM nodes WHERE type IN ('summary','digest') ORDER BY id DESC LIMIT ?`,
		limit)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("recent summaries: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			span.RecordError(err)
			return nil, fmt.Errorf("scan recent summary: %w", err)
		}
		out = append(out, content)
	}
	if err := rows.Err(); err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("iterate recent summaries: %w", err)
	}
	span.SetAttributes(attribute.Int("db.recent_count", len(out)))
	return out, nil
}

// CountSummariesSince returns the number of summary and digest nodes created after since. Used as a cost guard so the state deriver skips recomputation when nothing new has been written.
// since is formatted as UTC "2006-01-02 15:04:05" to match SQLite's CURRENT_TIMESTAMP storage format, which has no sub-second component.
func (s *Store) CountSummariesSince(ctx context.Context, since time.Time) (int, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.CountSummariesSince")
	defer span.End()

	// Truncate to second precision and use >= so rows inserted within the same clock-second as since are included — safe because production callers set since right after a derive run, so those rows should trigger a re-derive anyway.
	sinceStr := since.UTC().Truncate(time.Second).Format("2006-01-02 15:04:05")
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM nodes WHERE type IN ('summary','digest') AND created_at >= ?`,
		sinceStr).Scan(&n)
	if err != nil {
		span.RecordError(err)
		return 0, fmt.Errorf("count summaries since: %w", err)
	}
	span.SetAttributes(attribute.Int("db.count", n))
	return n, nil
}

// MemoryHit is one FTS5 row — either a summary or a note.
type MemoryHit struct {
	Content   string
	Source    string // "episode" | "summary" | "digest" | "note" | "thread"
	RefID     int64
	Domain    string    // work | personal | "" — set when known
	CreatedAt time.Time // zero if unknown
	App       string    // episode framing; empty for non-episodes
	Title     string
	ImagePath string // relative JPEG path when vision captured a frame
}

// excerptContent caps content to maxRunes (defaulting to maxEpisodeExcerpt when maxRunes <= 0) — shared by FormatHit and FormatNoteHit so every caller excerpts identically instead of each re-implementing the rune cap.
func excerptContent(content string, maxRunes int) string {
	if maxRunes <= 0 {
		maxRunes = maxEpisodeExcerpt
	}
	if runes := []rune(content); len(runes) > maxRunes {
		return string(runes[:maxRunes])
	}
	return content
}

// FormatHit renders a hit for the model in the established tool/inject shape:
//
//	[note] …
//	[summary] …
//	[episode] App — Title: …
//
// Source labels stay as stored (episode/note/summary/…) — kind-aware ranking happens in HybridSearch/RankedEpisodes, not in the prefix.
// Episode hits carry App/Title provenance (which window/app the capture came from) — without it the model can't tell two episodes from different, unrelated projects apart, which is exactly how cross-project confabulation happens (see systemInstructionText's synthesis-rule comment, WP12 Part C). Other sources are left as they were: threads/summaries/digests already carry their own framing (subject, kind) baked into content itself.
// Content is excerpted for inject budget; full content still lives in the DB.
func FormatHit(h MemoryHit, maxRunes int) string {
	content := excerptContent(h.Content, maxRunes)
	if h.Source == "note" {
		return fmt.Sprintf("[note] %s", content)
	}
	src := h.Source
	if src == "" {
		src = "unknown"
	}
	// Threads carry their ref id for the same reason notes do: a thread's summary can be wrong, the model can see that it is, and fix_thread needs an id to name. Nothing else here has a repair tool.
	if h.Source == "thread" && h.RefID > 0 {
		src = fmt.Sprintf("thread#%d", h.RefID)
	}
	label := src
	if age := formatRelativeAge(h.CreatedAt); age != "" {
		label = fmt.Sprintf("%s (%s)", src, age)
	}
	// Content leads and the window provenance trails in a parenthetical: the model reads the row left to right, so what it should say comes first and the machine names it should not say come last (taste audit T1 — the provenance still guards cross-project confabulation, it just stops being the headline).
	if h.Source == "episode" && (h.App != "" || h.Title != "") {
		line := fmt.Sprintf("[%s] %s (%s — %s)", label, content, h.App, h.Title)
		if h.ImagePath != "" {
			line += " [img]"
		}
		return line
	}
	return fmt.Sprintf("[%s] %s", label, content)
}

// formatRelativeAge renders how long ago t was, in the coarsest unit that's still informative ("3d ago", "5h ago", "2m ago") — "" for a zero/unknown time, so FormatHit can skip the suffix entirely rather than print a meaningless age.
func formatRelativeAge(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	age := time.Since(t)
	if age < 0 {
		age = 0
	}
	switch {
	case age < time.Hour:
		m := int(age.Minutes())
		if m < 1 {
			m = 1
		}
		return fmt.Sprintf("%dm ago", m)
	case age < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(age.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(age.Hours()/24))
	}
}

// FormatNoteHit renders a note hit with its ref_id in the "[note#N] …" shape — notes are the only source with an update_note/delete_note follow-up tool, so a caller (query_memory) needs the id in hand to act on a correction. Content is excerpted identically to FormatHit.
func FormatNoteHit(h MemoryHit, maxRunes int) string {
	return fmt.Sprintf("[note#%d] %s", h.RefID, excerptContent(h.Content, maxRunes))
}

// ftsStopwords are short function words dropped when tokenizing a query for buildFTSMatch — they carry no retrieval signal and just add noise to the OR-matched term set.
var ftsStopwords = map[string]bool{
	"a": true, "an": true, "the": true, "is": true, "are": true, "was": true,
	"were": true, "what": true, "when": true, "where": true, "did": true,
	"do": true, "does": true, "i": true, "you": true, "my": true, "your": true,
	"it": true, "its": true, "and": true, "or": true, "of": true, "to": true,
	"in": true, "on": true, "for": true, "that": true, "this": true,
}

// ftsQuotePhrase wraps a single FTS5 phrase in quotes, doubling any embedded quotes so it can't break out of the phrase.
func ftsQuotePhrase(phrase string) string {
	return `"` + strings.ReplaceAll(phrase, `"`, `""`) + `"`
}

// tokenizeQuery splits query into significant terms: whitespace/punctuation-separated, lowercased, deduped, with short function words (ftsStopwords) dropped — they carry no retrieval signal. Shared by buildFTSMatch (which ORs the terms into an FTS5 MATCH expression) and HybridSearch's lexical relevance floor (which counts how many of them a candidate's content actually contains).
func tokenizeQuery(query string) []string {
	raw := strings.FieldsFunc(query, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	seen := make(map[string]bool, len(raw))
	var terms []string
	for _, term := range raw {
		term = strings.ToLower(term)
		if term == "" || ftsStopwords[term] || seen[term] {
			continue
		}
		seen[term] = true
		terms = append(terms, term)
	}
	return terms
}

// buildFTSMatch tokenizes query (see tokenizeQuery) and joins the terms into an FTS5 OR-of-terms MATCH expression (e.g. `"foo" OR "bar"`). Natural-language questions rarely share a verbatim contiguous phrase with stored prose, so matching on any significant term recalls far better than treating the whole query as one phrase.
// Falls back to the original whole-query phrase when tokenization leaves no terms (e.g. an all-stopword query), so the call never MATCHes on an empty/invalid expression.
func buildFTSMatch(query string) string {
	terms := tokenizeQuery(query)
	if len(terms) == 0 {
		return ftsQuotePhrase(query)
	}
	parts := make([]string, len(terms))
	for i, term := range terms {
		parts[i] = ftsQuotePhrase(term)
	}
	return strings.Join(parts, " OR ")
}

// SearchMemory runs FTS5 over summaries + notes. Returns top 10 by rank.
// Empty query -> empty result, no error.
func (s *Store) SearchMemory(ctx context.Context, query string) ([]MemoryHit, error) {
	return s.searchMemoryWindow(ctx, query, time.Time{}, time.Time{})
}

// sqliteUTC renders t the way every timestamp column in this store is written (UTC "YYYY-MM-DD HH:MM:SS"), so bound parameters compare correctly against stored values.
func sqliteUTC(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05")
}

// ftsRowTime is the SQL expression for a memory_fts row's timestamp — the fts table itself carries none, so it is looked up in the row's source table: notes.created_at, threads.last_seen_at (a thread's meaningful time is when it was last touched — same choice hitCreatedAt makes), or nodes.created_at for summary/digest. NULL for a dangling ref, which any comparison then excludes.
const ftsRowTime = `(CASE source
	WHEN 'note' THEN (SELECT created_at FROM notes WHERE id = ref_id)
	WHEN 'thread' THEN (SELECT last_seen_at FROM threads WHERE id = ref_id)
	ELSE (SELECT created_at FROM nodes WHERE id = ref_id)
END)`

// searchMemoryWindow is SearchMemory constrained to rows whose timestamp falls in [since, until]; a zero bound is open on that side. The window is part of the WHERE clause, before the LIMIT, so a sparse window still yields its rows instead of being crowded out by out-of-window rows that rank higher.
func (s *Store) searchMemoryWindow(ctx context.Context, query string, since, until time.Time) ([]MemoryHit, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.SearchMemory")
	defer span.End()

	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}

	// MATCH wants tokens, not one whole-query phrase; OR the significant terms.
	safe := buildFTSMatch(query)

	where := "memory_fts MATCH ?"
	args := []any{safe}
	if !since.IsZero() {
		where += " AND " + ftsRowTime + " >= ?"
		args = append(args, sqliteUTC(since))
	}
	if !until.IsZero() {
		where += " AND " + ftsRowTime + " <= ?"
		args = append(args, sqliteUTC(until))
	}

	// Summary rows carry the whole marshalled TaskSummary in content (see LogSemanticNode), so the plain summary text is pulled back out here — the model reads a hit's Content verbatim and a raw JSON blob is unreadable. Digest rows and any summary whose content isn't JSON fall through unchanged.
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			CASE WHEN source IN ('summary','digest') AND json_valid(content)
				THEN IFNULL(NULLIF(json_extract(content, '$.summary'), ''), content)
				ELSE content
			END,
			source, ref_id
		FROM memory_fts
		WHERE `+where+`
		ORDER BY rank
		LIMIT 10
	`, args...)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("fts5 search: %w", err)
	}
	defer rows.Close()

	var out []MemoryHit
	for rows.Next() {
		var h MemoryHit
		if err := rows.Scan(&h.Content, &h.Source, &h.RefID); err != nil {
			span.RecordError(err)
			return nil, fmt.Errorf("scan fts5 row: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("iterate fts5 rows: %w", err)
	}
	rows.Close()

	// A second pass, after the FTS cursor above is fully closed — a follow-up query issued while it's still open can silently fail depending on connection-pool state, leaving CreatedAt at its zero value.
	for i := range out {
		out[i].CreatedAt = s.hitCreatedAt(ctx, out[i].Source, out[i].RefID)
	}

	span.SetAttributes(attribute.Int("db.search_results", len(out)))
	return out, nil
}

// hitCreatedAt looks up when a memory_fts-backed hit's row was created — memory_fts itself carries no timestamp column, so this is a follow-up query per row (bounded: SearchMemory caps at 10 rows). "thread" and any other unrecognized source return the zero value; FormatHit already treats a zero CreatedAt as "no age known."
func (s *Store) hitCreatedAt(ctx context.Context, source string, refID int64) time.Time {
	var created string
	switch source {
	case "summary", "digest":
		_ = s.db.QueryRowContext(ctx, `SELECT created_at FROM nodes WHERE id = ?`, refID).Scan(&created)
	case "note":
		_ = s.db.QueryRowContext(ctx, `SELECT created_at FROM notes WHERE id = ?`, refID).Scan(&created)
	case "thread":
		// last_seen_at, not created_at: a thread's age that matters is when it was last touched, which is also what MemoryAsOf reports for one.
		_ = s.db.QueryRowContext(ctx, `SELECT last_seen_at FROM threads WHERE id = ?`, refID).Scan(&created)
	default:
		return time.Time{}
	}
	return parseSQLiteTime(created)
}

// maxEpisodeExcerpt caps how much of an episode's screen_text is surfaced in RetrieveRelevant output — this is context meant to orient the model, not a full transcript.
const maxEpisodeExcerpt = 200

// recentTaskWindow bounds how far back GetImplicitContext looks when folding recent task names into its focus signal. Without a time bound, "last 2 tasks by id" in a lightly-used store can still be a stale task from days ago that self-matches its own summary back into context regardless of relevance.
const recentTaskWindow = 2 * time.Hour

// Ranking weights for RankedEpisodes. All three terms are min-max normalized to [0,1] across the candidate set before weighting, so these are directly comparable "how much each signal counts" knobs. Equal by default (combined score ranges [0,3]) — tune here if one signal should dominate.
const (
	rankWeightRecency    = 1.0
	rankWeightImportance = 1.0
	rankWeightRelevance  = 1.0
)

// recencyHalfLifeFactor is the per-hour exponential decay base for recency scoring: recency = recencyHalfLifeFactor^hoursSinceCreated. Closer to 1.0 means slower decay (0.995 ≈ half over ~138 hours / ~5.75 days).
const recencyHalfLifeFactor = 0.995

// rankCandidatePoolSize bounds how many FTS matches are pulled from episodes_fts before re-ranking in Go — wider than the final `limit` so the weighted formula, not raw FTS rank alone, decides the final order.
const rankCandidatePoolSize = 50

// rankedEpisodeCandidate holds the raw per-episode signals needed to compute a combined ranking score before normalization.
type rankedEpisodeCandidate struct {
	id         int64
	content    string
	createdAt  time.Time
	importance float64
	bm25       float64 // raw FTS5 bm25 score; more negative = better match
}

// RankedEpisodes scores episodes matching focus by a weighted blend of recency, importance, and FTS relevance, and returns the top `limit` as MemoryHit (Source="episode"). Each term is min-max normalized to [0,1] across the candidate pool before weighting:
//
//	score = rankWeightRecency*recency + rankWeightImportance*importance + rankWeightRelevance*relevance
//
// recency = recencyHalfLifeFactor^hoursSinceCreated (higher = more recent), importance is the stored episodes.importance column, and relevance is the FTS5 bm25 score inverted (bm25 is "lower is better") then normalized so the best match in the pool scores 1.0.
func (s *Store) RankedEpisodes(ctx context.Context, focus string, limit int) ([]MemoryHit, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.RankedEpisodes")
	defer span.End()

	focus = strings.TrimSpace(focus)
	if focus == "" || limit <= 0 {
		return nil, nil
	}
	safe := `"` + strings.ReplaceAll(focus, `"`, `""`) + `"`

	rows, err := s.db.QueryContext(ctx, `
		SELECT episodes.id, episodes.screen_text, episodes.created_at, episodes.importance,
		       episodes.app, episodes.title, episodes.domain, bm25(episodes_fts)
		FROM episodes_fts
		JOIN episodes ON episodes.id = episodes_fts.rowid
		WHERE episodes_fts MATCH ?
		ORDER BY rank
		LIMIT ?
	`, safe, rankCandidatePoolSize)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("fts5 ranked episode candidates: %w", err)
	}
	defer rows.Close()

	type rankedRow struct {
		rankedEpisodeCandidate
		app, title, domain string
	}
	var candidates []rankedRow
	for rows.Next() {
		var c rankedRow
		if err := rows.Scan(&c.id, &c.content, &c.createdAt, &c.importance, &c.app, &c.title, &c.domain, &c.bm25); err != nil {
			span.RecordError(err)
			return nil, fmt.Errorf("scan ranked episode candidate: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("iterate ranked episode candidates: %w", err)
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	now := time.Now()
	recencyRaw := make([]float64, len(candidates))
	relevanceRaw := make([]float64, len(candidates))
	for i, c := range candidates {
		hours := now.Sub(c.createdAt).Hours()
		if hours < 0 {
			hours = 0
		}
		recencyRaw[i] = math.Pow(recencyHalfLifeFactor, hours)
		relevanceRaw[i] = -c.bm25 // invert: higher = more relevant
	}

	recencyNorm := minMaxNormalize(recencyRaw)
	relevanceNorm := minMaxNormalize(relevanceRaw)
	importanceRaw := make([]float64, len(candidates))
	for i, c := range candidates {
		importanceRaw[i] = c.importance
	}
	importanceNorm := minMaxNormalize(importanceRaw)

	type scored struct {
		hit   MemoryHit
		score float64
	}
	results := make([]scored, len(candidates))
	for i, c := range candidates {
		results[i] = scored{
			hit: MemoryHit{
				Content:   c.content,
				Source:    "episode",
				RefID:     c.id,
				App:       c.app,
				Title:     c.title,
				Domain:    c.domain,
				CreatedAt: c.createdAt,
			},
			score: rankWeightRecency*recencyNorm[i] + rankWeightImportance*importanceNorm[i] + rankWeightRelevance*relevanceNorm[i],
		}
	}

	sort.SliceStable(results, func(i, j int) bool {
		return results[i].score > results[j].score
	})

	if limit < len(results) {
		results = results[:limit]
	}

	out := make([]MemoryHit, len(results))
	for i, r := range results {
		out[i] = r.hit
	}
	span.SetAttributes(attribute.Int("db.ranked_episode_count", len(out)))
	return out, nil
}

// minMaxNormalize scales vals to [0,1]. When all values are equal (max==min), every element normalizes to 1.0 so that term contributes its full weight uniformly rather than collapsing the whole score to 0.
func minMaxNormalize(vals []float64) []float64 {
	out := make([]float64, len(vals))
	if len(vals) == 0 {
		return out
	}
	min, max := vals[0], vals[0]
	for _, v := range vals {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	if max == min {
		for i := range out {
			out[i] = 1.0
		}
		return out
	}
	for i, v := range vals {
		out[i] = (v - min) / (max - min)
	}
	return out
}

// RetrieveRelevant returns up to maxItems relevance-ranked lines for unsolicited inject. Uses HybridSearch so moments get recency decay, facts/periods do not, and every line is FormatHit (content + context).
// If maxItems <= 0, defaults to 4 (stingy inject budget). An empty focus returns nil directly — there's no real query to run relevance search against, and substituting a placeholder phrase would just search for those literal words.
func (s *Store) RetrieveRelevant(ctx context.Context, focus string, maxItems int) ([]string, error) {
	focus = strings.TrimSpace(focus)
	if focus == "" {
		return nil, nil
	}
	if maxItems <= 0 {
		maxItems = 4
	}
	hits, err := s.HybridSearch(ctx, focus, "", maxItems)
	if err != nil {
		return nil, err
	}
	if len(hits) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, FormatHit(h, maxEpisodeExcerpt))
	}
	return out, nil
}

// RelevantNotes returns up to limit note contents relevant to focus, instead of the full notes table — same relevance-gated shape RetrieveRelevant/GetImplicitContext already use, applied to the plain fact strings DeriveState expects (no "[note]" prefix). An empty focus returns nil directly, same reasoning as RetrieveRelevant.
func (s *Store) RelevantNotes(ctx context.Context, focus string, limit int) ([]string, error) {
	focus = strings.TrimSpace(focus)
	if focus == "" {
		return nil, nil
	}
	hits, err := s.SearchMemory(ctx, focus)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, h := range hits {
		if h.Source != "note" {
			continue
		}
		if limit > 0 && len(out) >= limit {
			break
		}
		out = append(out, h.Content)
	}
	return out, nil
}

// DeleteNote removes a note by id. FTS5 mirror is dropped via trigger. Its vector (if any) is deleted async/best-effort — same non-blocking pattern as LogNote's embed goroutine — so a vector-index error never fails the SQL delete the model is waiting on.
func (s *Store) DeleteNote(ctx context.Context, id int64) error {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.DeleteNote")
	defer span.End()

	_, err := s.db.ExecContext(ctx, `DELETE FROM notes WHERE id = ?`, id)
	if err != nil {
		span.RecordError(err)
		return err
	}

	s.mu.RLock()
	vidx := s.vectorIndex
	s.mu.RUnlock()
	if vidx != nil {
		go func(id int64) {
			delCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := vidx.Delete(delCtx, fmt.Sprintf("note:%d", id)); err != nil {
				slog.Error("async note vector delete failed", "note_id", id, "error", err)
			}
		}(id)
	}
	return nil
}

// UpdateNote overwrites the content of an existing note. FTS5 mirror is kept in sync via the notes_au trigger, and updated_at is refreshed atomically. The stale vector is deleted and the corrected content re-embedded async/best-effort, same non-blocking pattern as LogNote — a vector-index error never fails the SQL update.
func (s *Store) UpdateNote(ctx context.Context, id int64, content string) error {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.UpdateNote")
	defer span.End()

	res, err := s.db.ExecContext(ctx,
		`UPDATE notes SET content = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		content, id)
	if err != nil {
		span.RecordError(err)
		return fmt.Errorf("update note: %w", err)
	}
	span.SetAttributes(attribute.Int64("db.note_id", id))

	// A no-op UPDATE is not success: the model can hand us an id it invented, and reporting "updated" throws the user's correction away. Returning before the vector work below also stops the "note:<id>" Delete from firing on an id that may belong to some other real note.
	if n, rerr := res.RowsAffected(); rerr != nil {
		span.RecordError(rerr)
		return fmt.Errorf("update note: %w", rerr)
	} else if n == 0 {
		err := fmt.Errorf("no note with id %d", id)
		span.RecordError(err)
		return err
	}

	s.mu.RLock()
	emb, vidx := s.embedder, s.vectorIndex
	s.mu.RUnlock()
	if vidx != nil {
		go func(id int64, text string) {
			vecCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			vecID := fmt.Sprintf("note:%d", id)
			if err := vidx.Delete(vecCtx, vecID); err != nil {
				slog.Error("async note vector delete (pre-update) failed", "note_id", id, "error", err)
			}
			if emb == nil || strings.TrimSpace(text) == "" {
				return
			}
			vec, err := emb.Embed(vecCtx, "RETRIEVAL_DOCUMENT", text)
			if err != nil {
				slog.Error("async note re-embed failed", "note_id", id, "error", err)
				return
			}
			meta := map[string]string{
				"source":     "note",
				"kind":       string(memory.KindFact),
				"created_at": time.Now().UTC().Format(time.RFC3339),
			}
			if err := vidx.Add(vecCtx, vecID, text, vec, meta); err != nil {
				slog.Error("async note vector re-add failed", "note_id", id, "error", err)
			}
		}(id, content)
	}
	return nil
}

// ExistingNotes returns id+content for every stored note of kind "fact". Used by the memory compiler to feed the reconciliation LLM call and by note consolidation to feed the curation call.
// Both of those calls hand the notes to a model whose job is to merge and drop entries, and both write the result back through ReplaceAllNotes, so only the kind they are allowed to rewrite is shown to them. Other kinds — meeting minutes above all, which are the only record of what was said in a call — are never offered up for curation.
func (s *Store) ExistingNotes(ctx context.Context) ([]memory.NoteRef, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.ExistingNotes")
	defer span.End()

	rows, err := s.db.QueryContext(ctx, `SELECT id, content FROM notes WHERE kind = 'fact' ORDER BY id ASC`)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("query existing notes: %w", err)
	}
	defer rows.Close()

	var out []memory.NoteRef
	for rows.Next() {
		var n memory.NoteRef
		if err := rows.Scan(&n.ID, &n.Content); err != nil {
			span.RecordError(err)
			return nil, fmt.Errorf("scan note ref: %w", err)
		}
		out = append(out, n)
	}
	span.SetAttributes(attribute.Int("db.note_count", len(out)))
	return out, nil
}

// UpsertThread creates or refreshes a thread, returning its id. For an existing id it bumps salience/recency in place; for a new one it upserts on (subject, kind) so the same throughline is recognized over time.
func (s *Store) UpsertThread(ctx context.Context, u memory.ThreadUpdate) (int64, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.UpsertThread")
	defer span.End()

	if u.ID > 0 {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE threads SET state=?, last_seen_at=CURRENT_TIMESTAMP, times_seen=times_seen+1, salience=MIN(1.0, salience+0.05), status='active' WHERE id=?`,
			u.State, u.ID); err != nil {
			span.RecordError(err)
			return 0, fmt.Errorf("update thread: %w", err)
		}
		span.SetAttributes(attribute.Int64("db.thread_id", u.ID))
		return u.ID, nil
	}

	// new throughline: bias salience up slightly when the model flags it novel.
	salience := 0.5
	if u.Novel {
		salience = 0.6
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO threads(subject,kind,state,salience,times_seen) VALUES(?,?,?,?,1)
		 ON CONFLICT(subject,kind) DO UPDATE SET state=excluded.state, last_seen_at=CURRENT_TIMESTAMP, times_seen=threads.times_seen+1, salience=MIN(1.0, threads.salience+0.05), status='active'`,
		u.Subject, u.Kind, u.State, salience); err != nil {
		span.RecordError(err)
		return 0, fmt.Errorf("insert thread: %w", err)
	}

	var id int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT id FROM threads WHERE subject=? AND kind=?`,
		u.Subject, u.Kind).Scan(&id); err != nil {
		span.RecordError(err)
		return 0, fmt.Errorf("read thread id: %w", err)
	}
	span.SetAttributes(attribute.Int64("db.thread_id", id))
	return id, nil
}

// UpdateThreadState overwrites an existing thread's state — the one-line summary of where that throughline stands — leaving its subject and kind alone. This is the repair path for a thread whose summary merged two unrelated things or recorded a wrong fact; the model can see that from a "[thread#N]" hit but had no way to act on it, since update_note only reaches the notes table.
// The FTS5 mirror is kept in sync by the threads_au trigger. The stale vector is deleted and the corrected text re-embedded async/best-effort, same non-blocking pattern as UpdateNote — a vector-index error never fails the SQL update. The embed text is "subject — state", matching the threads_ai trigger so both halves of hybrid search see the same thread.
// Input: the thread's id and the corrected state. Output: an error if no thread carries that id.
func (s *Store) UpdateThreadState(ctx context.Context, id int64, state string) error {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.UpdateThreadState")
	defer span.End()
	span.SetAttributes(attribute.Int64("db.thread_id", id))

	var subject string
	// A no-op UPDATE is not success: the model can hand us an id it invented, and reporting the fix as done throws the user's correction away. Reading the subject first both catches that and gives the re-embed its text.
	if err := s.db.QueryRowContext(ctx, `SELECT subject FROM threads WHERE id = ?`, id).Scan(&subject); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("no thread with id %d", id)
		}
		span.RecordError(err)
		return fmt.Errorf("update thread state: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE threads SET state = ?, last_seen_at = CURRENT_TIMESTAMP WHERE id = ?`, state, id); err != nil {
		span.RecordError(err)
		return fmt.Errorf("update thread state: %w", err)
	}

	s.mu.RLock()
	emb, vidx := s.embedder, s.vectorIndex
	s.mu.RUnlock()
	if vidx == nil {
		return nil
	}
	text := subject
	if strings.TrimSpace(state) != "" {
		text = subject + " — " + state
	}
	go func(id int64, text string) {
		vecCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		vecID := fmt.Sprintf("thread:%d", id)
		if err := vidx.Delete(vecCtx, vecID); err != nil {
			slog.Error("async thread vector delete (pre-update) failed", "thread_id", id, "error", err)
		}
		if emb == nil {
			return
		}
		vec, err := emb.Embed(vecCtx, "RETRIEVAL_DOCUMENT", text)
		if err != nil {
			slog.Error("async thread re-embed failed", "thread_id", id, "error", err)
			return
		}
		meta := map[string]string{
			"source":     "thread",
			"kind":       string(memory.KindArc),
			"created_at": time.Now().UTC().Format(time.RFC3339),
		}
		if err := vidx.Add(vecCtx, vecID, text, vec, meta); err != nil {
			slog.Error("async thread vector re-add failed", "thread_id", id, "error", err)
		}
	}(id, text)
	return nil
}

// GetLiveThreads returns threads touched in the last 2 days, newest-first. This is the "what's going on in their life right now" view; concurrent threads coexist here.
func (s *Store) GetLiveThreads(ctx context.Context, limit int) ([]memory.Thread, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.GetLiveThreads")
	defer span.End()

	return s.queryThreads(ctx, span,
		`SELECT id,subject,kind,IFNULL(state,''),salience,times_seen,last_seen_at,status FROM threads WHERE last_seen_at >= datetime('now','-2 days') ORDER BY last_seen_at DESC LIMIT ?`,
		limit)
}

// ThreadsForAttribution returns threads touched in the last 14 days, newest-first. Wider window than GetLiveThreads so the compiler can reattach to a throughline the user picked back up after a few days away.
func (s *Store) ThreadsForAttribution(ctx context.Context, limit int) ([]memory.Thread, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.ThreadsForAttribution")
	defer span.End()

	return s.queryThreads(ctx, span,
		`SELECT id,subject,kind,IFNULL(state,''),salience,times_seen,last_seen_at,status FROM threads WHERE last_seen_at >= datetime('now','-14 days') ORDER BY last_seen_at DESC LIMIT ?`,
		limit)
}

// queryThreads runs a thread SELECT and scans rows. last_seen_at scans into a time.Time, matching how GetNotes scans Note.CreatedAt.
func (s *Store) queryThreads(ctx context.Context, span trace.Span, query string, limit int) ([]memory.Thread, error) {
	rows, err := s.db.QueryContext(ctx, query, limit)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("query threads: %w", err)
	}
	defer rows.Close()

	var out []memory.Thread
	for rows.Next() {
		var t memory.Thread
		if err := rows.Scan(&t.ID, &t.Subject, &t.Kind, &t.State, &t.Salience, &t.TimesSeen, &t.LastSeen, &t.Status); err != nil {
			span.RecordError(err)
			return nil, fmt.Errorf("scan thread: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("iterate threads: %w", err)
	}
	span.SetAttributes(attribute.Int("db.thread_count", len(out)))
	return out, nil
}

// ReplaceAllNotes atomically swaps every note of kind "fact" for a curated set, used by periodic note consolidation. The new notes are written as kind "fact" too; FTS5 mirror stays in sync via the per-row notes_ad / notes_ai triggers.
// Notes of any other kind are left exactly as they are, rows and vectors both: meeting minutes live in this table under kind "meeting" and are the only record of what was said in a call, so consolidation must not be able to reach them.
// The caller must guarantee contents is non-empty — an empty swap would wipe the facts — but we defend against it here too.
func (s *Store) ReplaceAllNotes(ctx context.Context, contents []string) error {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.ReplaceAllNotes")
	defer span.End()

	if len(contents) == 0 {
		return fmt.Errorf("replace all notes: refusing to wipe table with empty set")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		span.RecordError(err)
		return fmt.Errorf("replace notes: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck — no-op after a successful Commit

	// Selected before the DELETE below so the old ids' vectors can be cleaned up — ReplaceAllNotes renumbers the facts it replaces (new AUTOINCREMENT ids on re-insert), so every replaced note's vector would otherwise become a permanent orphan. The kind filter matches the DELETE exactly: a vector is only deleted when its row is.
	var oldIDs []int64
	idRows, err := tx.QueryContext(ctx, `SELECT id FROM notes WHERE kind = 'fact'`)
	if err != nil {
		span.RecordError(err)
		return fmt.Errorf("replace notes: select old ids: %w", err)
	}
	for idRows.Next() {
		var id int64
		if err := idRows.Scan(&id); err != nil {
			idRows.Close()
			span.RecordError(err)
			return fmt.Errorf("replace notes: scan old id: %w", err)
		}
		oldIDs = append(oldIDs, id)
	}
	idRows.Close()
	if err := idRows.Err(); err != nil {
		span.RecordError(err)
		return fmt.Errorf("replace notes: iterate old ids: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM notes WHERE kind = 'fact'`); err != nil {
		span.RecordError(err)
		return fmt.Errorf("replace notes: clear: %w", err)
	}

	for _, c := range contents {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO notes (content, kind) VALUES (?, 'fact')`, c); err != nil {
			span.RecordError(err)
			return fmt.Errorf("replace notes: insert: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		span.RecordError(err)
		return fmt.Errorf("replace notes: commit: %w", err)
	}
	span.SetAttributes(attribute.Int("db.note_count", len(contents)))

	s.mu.RLock()
	vidx := s.vectorIndex
	s.mu.RUnlock()
	if vidx != nil && len(oldIDs) > 0 {
		go func(ids []int64) {
			delCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			for _, id := range ids {
				if err := vidx.Delete(delCtx, fmt.Sprintf("note:%d", id)); err != nil {
					slog.Error("async note vector delete (replace-all) failed", "note_id", id, "error", err)
				}
			}
		}(oldIDs)
	}
	return nil
}

// get or create
func (s *Store) ensureNode(ctx context.Context, parentID int64, nodeType, content string) (int64, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	_, span := tracer.Start(ctx, "DB.EnsureNode")
	span.SetAttributes(attribute.String("node.type", nodeType))
	defer span.End()

	var id int64

	// wrap in a transaction to ensure atomicity
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	if parentID == 0 {
		// ROOT, i.e. user
		// insert if not there already, or ignore and move on
		query := `INSERT OR IGNORE INTO nodes (type, content) VALUES (?, ?)`
		_, err = tx.Exec(query, nodeType, content)
		if err == nil {
			query = `SELECT id FROM nodes WHERE parent_id IS NULL AND type = ? AND content = ?`
			err = tx.QueryRow(query, nodeType, content).Scan(&id)
		}
	} else {
		// CHILD, i.e. day, session or activity
		query := `INSERT OR IGNORE INTO nodes (parent_id, type, content) VALUES (?, ?, ?)`
		_, err = tx.Exec(query, parentID, nodeType, content)
		if err == nil {
			query = `SELECT id FROM nodes WHERE parent_id = ? AND type = ? AND content = ?`
			err = tx.QueryRow(query, parentID, nodeType, content).Scan(&id)
		}
	}

	if err != nil {
		return 0, fmt.Errorf("failed to ensure %s node: %w", nodeType, err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("failed to commit transaction: %w", err)
	}

	return id, nil
}

// richnessWordCap is the word count at which screen_text richness saturates to 1.0 in computeImportance — beyond this point more words don't add signal.
const richnessWordCap = 150

// revisitLookbackWindow bounds how far back computeImportance looks for prior visits to the same app+title when scoring revisitation.
const revisitLookbackWindow = 7 * 24 * time.Hour

// revisitSaturationCount is the number of recent same-app+title episodes at which the revisitation term saturates to 1.0.
const revisitSaturationCount = 5

// computeImportance scores a new episode in [0,1] from two cheap, write-time signals, weighted equally (0.5 each):
//   - richness: word count of screen_text, normalized against richnessWordCap (capped at 1.0) — a fuller capture carries more information than a near-empty one.
//   - revisitation: how many of the last revisitSaturationCount episodes with the same app+title already exist within revisitLookbackWindow, normalized against revisitSaturationCount (capped at 1.0) — a place the user keeps coming back to is more likely to matter later.
func (s *Store) computeImportance(ctx context.Context, app, title, screenText string) (float64, error) {
	richness := float64(memory.CountWords(screenText)) / float64(richnessWordCap)
	if richness > 1 {
		richness = 1
	}

	var priorVisits int
	secs := int64(revisitLookbackWindow.Seconds())
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM episodes WHERE app = ? AND title = ? AND created_at >= datetime('now', '-' || ? || ' seconds')`,
		app, title, secs).Scan(&priorVisits)
	if err != nil {
		return 0, fmt.Errorf("count prior visits: %w", err)
	}
	revisitation := float64(priorVisits) / float64(revisitSaturationCount)
	if revisitation > 1 {
		revisitation = 1
	}

	return 0.5*richness + 0.5*revisitation, nil
}

// EpisodeWrite is one capture to persist. LogEpisode fills only App/Title/ScreenText; the daemon uses WriteEpisode when vision produced activity, visible chunks, or a JPEG.
type EpisodeWrite struct {
	App, Title, ScreenText string
	UserActivity           string
	VisibleText            []string
	ImageJPEG              []byte
	// ExtraJPEG holds one frame per monitor other than the one the user was on, captured at the same moment. They are stored beside the primary as {id}-b.jpg, {id}-c.jpg and read back with EpisodeExtraImages.
	ExtraJPEG [][]byte
}

// LogEpisode appends one dwell-confirmed capture to the episodes time series — a plain append, not a dedup: repeat visits to the same app+title MUST create distinct rows because screen_text differs between visits and is the whole point of capturing it.
func (s *Store) LogEpisode(ctx context.Context, app, title, screenText string) (int64, error) {
	return s.WriteEpisode(ctx, EpisodeWrite{App: app, Title: title, ScreenText: screenText})
}

// WriteEpisode is LogEpisode plus structured moment fields and an optional vision JPEG.
func (s *Store) WriteEpisode(ctx context.Context, w EpisodeWrite) (int64, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.LogEpisode")
	defer span.End()

	obs := memory.Normalize(w.App, w.Title, w.ScreenText)
	// Prefer normalized content; if normalize emptied a non-empty raw capture of only chrome, fall back to raw so we never invent empty rows that tests and AgeEpisodes still treat as real observations. The fallback passes through StripObjectChars so a titleless capture of pure U+FFFC placeholders cannot smuggle uncleaned text into storage.
	content := obs.Content
	if content == "" {
		content = memory.StripObjectChars(w.ScreenText)
	}
	if structured := memory.ComposeMoment(w.UserActivity, w.VisibleText, ""); structured != "" {
		content = structured
	}

	span.SetAttributes(
		attribute.String("db.app", obs.Context.App),
		attribute.String("db.window_title", obs.Context.Title),
		attribute.String("db.signal_kind", string(obs.Context.SignalKind)),
	)

	importance, err := s.computeImportance(ctx, obs.Context.App, obs.Context.Title, content)
	if err != nil {
		span.RecordError(err)
		return 0, fmt.Errorf("compute importance: %w", err)
	}

	domain := obs.Context.Domain

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO episodes (app, title, screen_text, importance, domain, user_activity, visible_text) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		obs.Context.App, obs.Context.Title, content, importance, string(domain),
		strings.TrimSpace(w.UserActivity), memory.VisibleTextJSON(w.VisibleText))
	if err != nil {
		span.RecordError(err)
		return 0, fmt.Errorf("insert episode: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		span.RecordError(err)
		return 0, fmt.Errorf("episode last insert id: %w", err)
	}
	span.SetAttributes(
		attribute.Int64("db.episode_id", id),
		attribute.Float64("db.episode_importance", importance),
		attribute.String("db.episode_domain", string(domain)),
	)

	if imgPath := s.writeEpisodeJPEG(id, w.ImageJPEG, w.ExtraJPEG); imgPath != "" {
		if _, err := s.db.ExecContext(ctx, `UPDATE episodes SET image_path = ? WHERE id = ?`, imgPath, id); err != nil {
			slog.Error("episode image path update failed", "episode_id", id, "error", err)
		}
	}

	// Async, best-effort embedding: LogEpisode must return immediately after the synchronous INSERT above (see TestLogEpisode_DoesNotBlockOnSlowEmbedder in db_test.go). The real network call happens inside this goroutine, so it deliberately uses its own context (30s timeout) instead of the caller's ctx — ctx may already be canceled by the time this goroutine runs, and canceling the embed with it would permanently lose that episode's vector-searchability.
	// Embed content+context (Document) even though screen_text is content-only.
	embedText := memory.Normalize(obs.Context.App, obs.Context.Title, content).Document()
	if strings.TrimSpace(embedText) == "" {
		embedText = content
	}
	s.mu.RLock()
	emb, vidx := s.embedder, s.vectorIndex
	s.mu.RUnlock()
	if emb != nil && vidx != nil && strings.TrimSpace(embedText) != "" {
		go func(id int64, text string, domain memory.Domain) {
			embedCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			vec, err := emb.Embed(embedCtx, "RETRIEVAL_DOCUMENT", text)
			if err != nil {
				slog.Error("async episode embed failed", "episode_id", id, "error", err)
				return
			}
			meta := map[string]string{
				"domain":     string(domain),
				"source":     "episode",
				"kind":       string(memory.KindMoment),
				"created_at": time.Now().UTC().Format(time.RFC3339),
			}
			if err := vidx.Add(embedCtx, fmt.Sprintf("episode:%d", id), text, vec, meta); err != nil {
				slog.Error("async episode vector index add failed", "episode_id", id, "error", err)
			}
		}(id, embedText, domain)
	}

	return id, nil
}

// Episode is one row of the episodes time series, exported for retrieval layers (the consolidation package) that need the full struct rather than just the MemoryHit projection.
type Episode struct {
	ID           int64
	CreatedAt    time.Time
	App          string
	Title        string
	ScreenText   string
	Importance   float64
	Domain       string
	UserActivity string
	VisibleText  string
	ImagePath    string
}

// EpisodeQuery is the filter for ListEpisodes. Zero Since/Until means unbounded on that side. App is a case-insensitive substring of episodes.app.
type EpisodeQuery struct {
	Since, Until time.Time
	App          string
	Limit        int
	NewestFirst  bool
}

// EpisodesInWindow returns episodes with created_at in [since, until], ordered chronologically (created_at ASC) — this is the "day arc" walk, letting a caller narrate what happened in the order it happened, unlike RankedEpisodes/SearchEpisodes which are relevance-ordered. Capped at limit.
func (s *Store) EpisodesInWindow(ctx context.Context, since, until time.Time, limit int) ([]Episode, error) {
	return s.ListEpisodes(ctx, EpisodeQuery{Since: since, Until: until, Limit: limit})
}

// ListEpisodes returns moments matching q. This is the SQL payload filter: app + time range + recency, without going through FTS or chromem.
func (s *Store) ListEpisodes(ctx context.Context, q EpisodeQuery) ([]Episode, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.ListEpisodes")
	defer span.End()

	if q.Limit <= 0 {
		return nil, nil
	}

	var clauses []string
	var args []any
	if !q.Since.IsZero() {
		clauses = append(clauses, "created_at >= ?")
		args = append(args, sqliteUTC(q.Since))
	}
	if !q.Until.IsZero() {
		clauses = append(clauses, "created_at <= ?")
		args = append(args, sqliteUTC(q.Until))
	}
	if app := strings.TrimSpace(q.App); app != "" {
		clauses = append(clauses, "LOWER(app) LIKE '%' || LOWER(?) || '%'")
		args = append(args, app)
	}
	where := ""
	if len(clauses) > 0 {
		where = "WHERE " + strings.Join(clauses, " AND ")
	}
	order := "created_at ASC"
	if q.NewestFirst {
		order = "created_at DESC"
	}
	args = append(args, q.Limit)

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, created_at, app, title, screen_text, importance, domain,
		       user_activity, visible_text, image_path
		FROM episodes
		`+where+`
		ORDER BY `+order+`
		LIMIT ?`, args...)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("list episodes: %w", err)
	}
	defer rows.Close()

	var out []Episode
	for rows.Next() {
		var e Episode
		if err := rows.Scan(&e.ID, &e.CreatedAt, &e.App, &e.Title, &e.ScreenText, &e.Importance, &e.Domain, &e.UserActivity, &e.VisibleText, &e.ImagePath); err != nil {
			span.RecordError(err)
			return nil, fmt.Errorf("scan episode: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("iterate episodes in window: %w", err)
	}
	span.SetAttributes(attribute.Int("db.episodes_in_window", len(out)))
	return out, nil
}

// mmrLambda trades off relevance vs. diversity in DiverseEpisodes' MMR-lite selection: score = mmrLambda*rel - (1-mmrLambda)*maxSimToChosen. Closer to 1.0 favors relevance (plain top-N); closer to 0 favors spread. 0.7 leans relevant but still lets a near-duplicate be passed over for something distinct.
const mmrLambda = 0.7

// mmrCandidatePoolSize bounds how many RankedEpisodes candidates feed the MMR selection in DiverseEpisodes.
const mmrCandidatePoolSize = 30

// mmrSameSubjectBump is the similarity score assigned when two candidates share the same (app,title) — treated as near-duplicate "the same visit" regardless of exact text, since repeat visits to the same window are the dominant source of redundancy in the episode stream.
const mmrSameSubjectBump = 0.9

// diverseEpisodeCandidate holds the per-candidate signals needed for MMR selection: the underlying hit, its (app,title) for the same-subject similarity bump, its pre-tokenized screen_text for Jaccard similarity, and its relevance score normalized from RankedEpisodes' rank position.
type diverseEpisodeCandidate struct {
	hit    MemoryHit
	app    string
	title  string
	tokens map[string]struct{}
	rel    float64
}

// tokenSet splits s on whitespace into a lowercased token set, for Jaccard similarity — no embeddings available, so this is the cheap substitute.
func tokenSet(s string) map[string]struct{} {
	fields := strings.Fields(strings.ToLower(s))
	set := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		set[f] = struct{}{}
	}
	return set
}

// jaccardSimilarity returns |a∩b| / |a∪b| over token sets, 0 when both are empty.
func jaccardSimilarity(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for tok := range a {
		if _, ok := b[tok]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// diverseSimilarity is the similarity(a,b) used by DiverseEpisodes' MMR selection: a strong same-(app,title) bump (same visit/subject, near-certain duplicate) or else token Jaccard over screen_text.
func diverseSimilarity(a, b diverseEpisodeCandidate) float64 {
	if a.app != "" && a.app == b.app && a.title == b.title {
		return mmrSameSubjectBump
	}
	return jaccardSimilarity(a.tokens, b.tokens)
}

// DiverseEpisodes selects up to limit episodes matching focus via MMR-lite (maximal marginal relevance): pull a candidate pool from RankedEpisodes, then iteratively pick the argmax of mmrLambda*rel - (1-mmrLambda)*maxSim, where rel is the candidate's normalized rank position and maxSim is its highest similarity (diverseSimilarity) to any already-chosen result. This avoids the near-duplicate pile-up a plain top-N would produce (e.g. 5 visits to the same app+title with near-identical text all scoring high).
func (s *Store) DiverseEpisodes(ctx context.Context, focus string, limit int) ([]MemoryHit, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.DiverseEpisodes")
	defer span.End()

	if limit <= 0 {
		return nil, nil
	}

	pool, err := s.RankedEpisodes(ctx, focus, mmrCandidatePoolSize)
	if err != nil {
		return nil, err
	}
	if len(pool) == 0 {
		return nil, nil
	}

	candidates := make([]diverseEpisodeCandidate, len(pool))
	for i, h := range pool {
		app, title := h.App, h.Title
		if app == "" && title == "" {
			// best-effort: if the lookup fails, app/title stay empty and the same-subject bump simply never fires for this candidate.
			_ = s.db.QueryRowContext(ctx, `SELECT app, title FROM episodes WHERE id = ?`, h.RefID).Scan(&app, &title)
		}
		candidates[i] = diverseEpisodeCandidate{
			hit:    h,
			app:    app,
			title:  title,
			tokens: tokenSet(h.Content),
			// rank position score: best-ranked candidate (i=0) scores highest.
			rel: float64(len(pool)-i) / float64(len(pool)),
		}
	}

	var selected []diverseEpisodeCandidate
	remaining := make([]int, len(candidates))
	for i := range remaining {
		remaining[i] = i
	}
	// Cap how many near-identical (app,title) moments may appear — document framing repeats title tokens so BM25 over-clusters revisits; without a hard cap, MMR alone can still fill the budget with one subject.
	const maxPerSubject = 1
	subjectCount := map[string]int{}

	for len(selected) < limit && len(remaining) > 0 {
		bestRemPos, bestIdx := -1, -1
		bestScore := math.Inf(-1)
		for pos, ci := range remaining {
			c := candidates[ci]
			subj := c.app + "\x00" + c.title
			if c.app != "" && subjectCount[subj] >= maxPerSubject {
				continue
			}
			maxSim := 0.0
			for _, sel := range selected {
				if sim := diverseSimilarity(c, sel); sim > maxSim {
					maxSim = sim
				}
			}
			score := mmrLambda*c.rel - (1-mmrLambda)*maxSim
			if score > bestScore {
				bestScore = score
				bestIdx = ci
				bestRemPos = pos
			}
		}
		if bestIdx < 0 {
			break // only over-quota subjects left
		}
		selected = append(selected, candidates[bestIdx])
		subj := candidates[bestIdx].app + "\x00" + candidates[bestIdx].title
		if candidates[bestIdx].app != "" {
			subjectCount[subj]++
		}
		remaining = append(remaining[:bestRemPos], remaining[bestRemPos+1:]...)
	}

	out := make([]MemoryHit, len(selected))
	for i, c := range selected {
		out[i] = c.hit
	}
	span.SetAttributes(attribute.Int("db.diverse_episode_count", len(out)))
	return out, nil
}

// RecallSubject fuses a thread's arc with episode specifics for "what do you know about X" recall: (a) matching thread(s) for subject via SearchMemory filtered to Source=="thread", formatted "[thread#N] <content>"; then (b) a diverse spread of matching episodes via DiverseEpisodes, formatted "[episode] <excerpt≤200 runes>". Threads (the throughline) come first, episodes (the specifics) after — narration should say "you've been doing X" before "specifically, Y and Z".
func (s *Store) RecallSubject(ctx context.Context, subject string, limit int) ([]string, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.RecallSubject")
	defer span.End()

	subject = strings.TrimSpace(subject)
	if subject == "" || limit <= 0 {
		return nil, nil
	}

	hits, err := s.SearchMemory(ctx, subject)
	if err != nil {
		return nil, err
	}

	var out []string
	for _, h := range hits {
		if h.Source == "thread" {
			out = append(out, fmt.Sprintf("[thread#%d] %s", h.RefID, h.Content))
		}
	}

	episodes, err := s.DiverseEpisodes(ctx, subject, limit)
	if err != nil {
		return nil, err
	}
	for _, h := range episodes {
		excerpt := h.Content
		if runes := []rune(excerpt); len(runes) > maxEpisodeExcerpt {
			excerpt = string(runes[:maxEpisodeExcerpt])
		}
		out = append(out, fmt.Sprintf("[episode] %s", excerpt))
	}

	span.SetAttributes(attribute.Int("db.recall_subject_lines", len(out)))
	return out, nil
}

// SearchEpisodes runs FTS5 MATCH over episodes_fts, returning the matching episodes' screen_text as MemoryHit.Content (Source="episode"), ordered by rank. Empty query -> empty result, no error, mirroring SearchMemory.
func (s *Store) SearchEpisodes(ctx context.Context, query string) ([]MemoryHit, error) {
	return s.searchEpisodesWindow(ctx, query, time.Time{}, time.Time{})
}

// searchEpisodesWindow is SearchEpisodes constrained to episodes whose created_at falls in [since, until]; a zero bound is open on that side. The window sits in the WHERE clause, before the LIMIT — see searchMemoryWindow for why.
func (s *Store) searchEpisodesWindow(ctx context.Context, query string, since, until time.Time) ([]MemoryHit, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.SearchEpisodes")
	defer span.End()

	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}

	safe := buildFTSMatch(query)

	where := "episodes_fts MATCH ?"
	args := []any{safe}
	if !since.IsZero() {
		where += " AND episodes.created_at >= ?"
		args = append(args, sqliteUTC(since))
	}
	if !until.IsZero() {
		where += " AND episodes.created_at <= ?"
		args = append(args, sqliteUTC(until))
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT episodes.screen_text, episodes.id, episodes.app, episodes.title,
		       episodes.domain, episodes.created_at, episodes.image_path
		FROM episodes_fts
		JOIN episodes ON episodes.id = episodes_fts.rowid
		WHERE `+where+`
		ORDER BY rank
		LIMIT 10
	`, args...)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("fts5 episode search: %w", err)
	}
	defer rows.Close()

	var out []MemoryHit
	for rows.Next() {
		var h MemoryHit
		var created string
		if err := rows.Scan(&h.Content, &h.RefID, &h.App, &h.Title, &h.Domain, &created, &h.ImagePath); err != nil {
			span.RecordError(err)
			return nil, fmt.Errorf("scan episode fts5 row: %w", err)
		}
		h.Source = "episode"
		h.CreatedAt = parseSQLiteTime(created)
		out = append(out, h)
	}
	span.SetAttributes(attribute.Int("db.episode_search_results", len(out)))
	return out, nil
}

// parseSQLiteTime accepts formats SQLite datetime('now') and RFC3339 commonly produce for episodes.created_at. Zero time on failure.
func parseSQLiteTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05Z",
		"2006-01-02T15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t
		}
	}
	return time.Time{}
}

// AgeEpisodes is the pre-vector tiering step for the episode substrate: for episodes older than keepRawFor whose importance is below importanceFloor, screen_text is emptied to reclaim space while the row itself (ts/app/title/importance) is kept — a thin record, not a deletion. Recent or high-importance episodes are left untouched. Rows are NEVER deleted; only screen_text is cleared (re-clearing an already-empty row is just a no-op). Returns the number of rows aged.
func (s *Store) AgeEpisodes(ctx context.Context, keepRawFor time.Duration, importanceFloor float64) (int64, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.AgeEpisodes")
	defer span.End()

	secs := int64(keepRawFor.Seconds())

	// Select the affected ids first (instead of one set-based UPDATE) so their vectors can be deleted too — AgeEpisodes clears screen_text specifically to reclaim that content, so leaving it live in the vector index would defeat the point.
	idRows, err := s.db.QueryContext(ctx,
		`SELECT id FROM episodes
		 WHERE created_at < datetime('now', '-' || ? || ' seconds')
		   AND importance < ?
		   AND screen_text != ''`,
		secs, importanceFloor)
	if err != nil {
		span.RecordError(err)
		return 0, fmt.Errorf("age episodes: select candidates: %w", err)
	}
	var ids []int64
	for idRows.Next() {
		var id int64
		if err := idRows.Scan(&id); err != nil {
			idRows.Close()
			span.RecordError(err)
			return 0, fmt.Errorf("age episodes: scan candidate: %w", err)
		}
		ids = append(ids, id)
	}
	idRows.Close()
	if err := idRows.Err(); err != nil {
		span.RecordError(err)
		return 0, fmt.Errorf("age episodes: iterate candidates: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}

	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE episodes SET screen_text = '', image_path = '' WHERE id IN (`+strings.Join(placeholders, ",")+`)`,
		args...)
	if err != nil {
		span.RecordError(err)
		return 0, fmt.Errorf("age episodes: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		span.RecordError(err)
		return 0, fmt.Errorf("age episodes rows affected: %w", err)
	}
	span.SetAttributes(attribute.Int64("db.aged_rows", n))

	s.deleteEpisodeVectors(ids)
	for _, id := range ids {
		s.removeEpisodeJPEG(id)
	}
	return n, nil
}

// AgeEpisodeImages deletes vision JPEGs older than keepFor and clears image_path. Descriptions, app/title, and the row stay. This is the storage cap for screenshots — 14 days of thumbnails, not a year of them.
func (s *Store) AgeEpisodeImages(ctx context.Context, keepFor time.Duration) (int64, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.AgeEpisodeImages")
	defer span.End()

	if keepFor <= 0 {
		return 0, nil
	}
	secs := int64(keepFor.Seconds())
	idRows, err := s.db.QueryContext(ctx,
		`SELECT id FROM episodes
		 WHERE created_at < datetime('now', '-' || ? || ' seconds')
		   AND image_path != ''`,
		secs)
	if err != nil {
		span.RecordError(err)
		return 0, fmt.Errorf("age episode images: select: %w", err)
	}
	var ids []int64
	for idRows.Next() {
		var id int64
		if err := idRows.Scan(&id); err != nil {
			idRows.Close()
			span.RecordError(err)
			return 0, fmt.Errorf("age episode images: scan: %w", err)
		}
		ids = append(ids, id)
	}
	idRows.Close()
	if err := idRows.Err(); err != nil {
		span.RecordError(err)
		return 0, fmt.Errorf("age episode images: iterate: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}

	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE episodes SET image_path = '' WHERE id IN (`+strings.Join(placeholders, ",")+`)`,
		args...)
	if err != nil {
		span.RecordError(err)
		return 0, fmt.Errorf("age episode images: update: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		span.RecordError(err)
		return 0, fmt.Errorf("age episode images: rows: %w", err)
	}
	for _, id := range ids {
		s.removeEpisodeJPEG(id)
	}
	span.SetAttributes(attribute.Int64("db.aged_images", n))
	return n, nil
}

// deleteEpisodeVectors deletes each id's "episode:N" vector async/best-effort, same non-blocking pattern as LogNote's embed goroutine — a vector-index error never fails the SQL op that reclaimed the row's raw text.
func (s *Store) deleteEpisodeVectors(ids []int64) {
	s.mu.RLock()
	vidx := s.vectorIndex
	s.mu.RUnlock()
	if vidx == nil {
		return
	}
	go func(ids []int64) {
		delCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, id := range ids {
			if err := vidx.Delete(delCtx, fmt.Sprintf("episode:%d", id)); err != nil {
				slog.Error("async episode vector delete failed", "episode_id", id, "error", err)
			}
		}
	}(ids)
}

// PruneAncientEpisodes is the coarse, long-horizon cap AgeEpisodes doesn't provide: AgeEpisodes only ever empties screen_text, so the episodes table's row count grows forever even once the expensive column is thinned. This deletes the row itself for episodes older than olderThan, but ONLY if screen_text is already empty — i.e. only rows that already went through AgeEpisodes (or were logged empty). Rows that still carry raw screen_text are never deleted here regardless of age, so this can never destroy text that hasn't already been through the aging pass.
//
// episodes_fts stays in sync via the existing episodes_ad AFTER DELETE trigger — no separate FTS cleanup needed here, since an already-thinned row's index entry was already collapsed to empty when AgeEpisodes ran.
//
// Returns the number of rows deleted.
func (s *Store) PruneAncientEpisodes(ctx context.Context, olderThan time.Duration) (int64, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.PruneAncientEpisodes")
	defer span.End()

	secs := int64(olderThan.Seconds())

	// Select the affected ids first (instead of one set-based DELETE) so any leftover vector for an already-thinned row gets cleaned up too.
	idRows, err := s.db.QueryContext(ctx,
		`SELECT id FROM episodes
		 WHERE created_at < datetime('now', '-' || ? || ' seconds')
		   AND screen_text = ''`,
		secs)
	if err != nil {
		span.RecordError(err)
		return 0, fmt.Errorf("prune ancient episodes: select candidates: %w", err)
	}
	var ids []int64
	for idRows.Next() {
		var id int64
		if err := idRows.Scan(&id); err != nil {
			idRows.Close()
			span.RecordError(err)
			return 0, fmt.Errorf("prune ancient episodes: scan candidate: %w", err)
		}
		ids = append(ids, id)
	}
	idRows.Close()
	if err := idRows.Err(); err != nil {
		span.RecordError(err)
		return 0, fmt.Errorf("prune ancient episodes: iterate candidates: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}

	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM episodes WHERE id IN (`+strings.Join(placeholders, ",")+`)`,
		args...)
	if err != nil {
		span.RecordError(err)
		return 0, fmt.Errorf("prune ancient episodes: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		span.RecordError(err)
		return 0, fmt.Errorf("prune ancient episodes rows affected: %w", err)
	}
	span.SetAttributes(attribute.Int64("db.pruned_rows", n))

	s.deleteEpisodeVectors(ids)
	for _, id := range ids {
		s.removeEpisodeJPEG(id)
	}
	return n, nil
}

func (s *Store) GetImplicitContext(ctx context.Context) ([]string, error) {
	// init tracer to db module
	tracer := obs.GetTracer(ctx, "ora.db")

	// starts the span, and will inherit a trace id from context
	ctx, span := tracer.Start(ctx, "GetImplicitContext")
	defer span.End()

	// recursive common table expression, kind of like a while loop
	// we grab the last 5 summaries and walk them up to the root (user/day/session)
	query := `WITH RECURSIVE branch AS (
		-- Anchor: The last 5 summaries under the current session
		SELECT * FROM (
			SELECT id, parent_id, type, content 
			FROM nodes 
			WHERE type = 'summary' AND parent_id IN (
				SELECT id FROM nodes WHERE parent_id = ? AND type = 'task'
			)
			ORDER BY id DESC LIMIT 5
		)
		
		UNION ALL	
		
		-- recursively walk up the tree
		SELECT n.id, n.parent_id, n.type, n.content 
		FROM nodes n
		JOIN branch b ON n.id = b.parent_id
	)
	-- DISTINCT to avoid repeating common ancestors (Session, Day, User)
	SELECT DISTINCT type, content FROM branch ORDER BY id ASC`

	s.mu.RLock()
	parentID := s.currentParentID
	s.mu.RUnlock()

	var branch []string
	// Identity notes are NOT dumped unconditionally anymore — a wall of generic "[about] user likes X" facts buried the useful live threads. Notes now surface only through relevance retrieval below, gated by what the user is actually doing now: build a focus signal from working state + recent task nodes, then surface only the notes/summaries/threads that match it.
	workingState, workingStateErr := s.GetWorkingState(ctx)
	var focusSignal string
	if workingStateErr == nil && workingState != "" {
		focusSignal = workingState
	}
	// Bounded by recency, not just by id: an old task is still "last 2 by id" in a lightly-used store, and folding its name into focusSignal would self-match its own summary back into context indefinitely regardless of whether it's actually current.
	recentSince := time.Now().Add(-recentTaskWindow).UTC().Format("2006-01-02 15:04:05")
	if trows, err := s.db.QueryContext(ctx, `SELECT content FROM nodes WHERE type='task' AND created_at >= ? ORDER BY id DESC LIMIT 2`, recentSince); err == nil && trows != nil {
		for trows.Next() {
			var tsk string
			_ = trows.Scan(&tsk)
			if tsk != "" {
				focusSignal += " " + tsk
			}
		}
		trows.Close()
	}
	// A cold-start store (no working state yet, no recent task) has no real focus signal — skip relevance search entirely instead of running it against a placeholder phrase (RetrieveRelevant now also returns nil on empty focus, but the point here is to never construct a fake non-empty one in the first place).
	if focusSignal = strings.TrimSpace(focusSignal); focusSignal != "" {
		const maxRel = 6
		if rel, rerr := s.RetrieveRelevant(ctx, focusSignal, maxRel); rerr == nil {
			branch = append(branch, rel...)
		}
	}
	// live threads: what's going on in their life right now (recency = relevance). Concurrent threads coexist here — watching + coding both surface.
	threads, terr := s.GetLiveThreads(ctx, 6)
	if terr == nil {
		for _, t := range threads {
			if t.State != "" {
				branch = append(branch, fmt.Sprintf("[thread:%s] %s — %s", t.Kind, t.Subject, t.State))
			} else {
				branch = append(branch, fmt.Sprintf("[thread:%s] %s", t.Kind, t.Subject))
			}
		}
	}
	// synthesized "right now" (reuses the working-state fetched above for focusSignal — nothing between there and here mutates it).
	if workingStateErr == nil && workingState != "" {
		branch = append(branch, fmt.Sprintf("[now] %s", workingState))
	}
	if len(branch) > 0 {
		span.SetAttributes(attribute.Int("db.node_count", len(branch)))
		return branch, nil
	}

	// fallback (cold start, nothing synthesized yet): existing recursive summary walk.
	rows, err := s.db.QueryContext(ctx, query, parentID)

	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("failed to query context: %w", err)
	}

	defer rows.Close()

	for rows.Next() {
		var nodeType, content string

		if err := rows.Scan(&nodeType, &content); err != nil {
			span.RecordError(err)
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}

		nodeString := fmt.Sprintf("[%s] %s", nodeType, content)
		branch = append(branch, nodeString)
	}

	if err := rows.Err(); err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("error iterating rows: %w", err)
	}

	span.SetAttributes(attribute.Int("db.node_count", len(branch)))
	return branch, nil
}

// OldSummaryGroups returns summary nodes older than olderThan, grouped by their ancestor DAY node. Used by the compaction job to decide which days are ready to roll up.
func (s *Store) OldSummaryGroups(ctx context.Context, olderThan time.Duration) ([]memory.SummaryGroup, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.OldSummaryGroups")
	defer span.End()

	secs := int64(olderThan.Seconds())
	rows, err := s.db.QueryContext(ctx, `
		SELECT day.id, day.content, sm.id, sm.content
		FROM nodes sm
		JOIN nodes task ON sm.parent_id = task.id AND task.type = 'task'
		JOIN nodes sess ON task.parent_id = sess.id AND sess.type = 'session'
		JOIN nodes day  ON sess.parent_id = day.id  AND day.type = 'day'
		WHERE sm.type = 'summary'
		  AND sm.created_at < datetime('now', '-' || ? || ' seconds')
		ORDER BY day.id, sm.id
	`, secs)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("old summary groups query: %w", err)
	}
	defer rows.Close()

	// build groups in insertion order while preserving day grouping
	var groups []memory.SummaryGroup
	index := make(map[int64]int) // dayID → index into groups slice

	for rows.Next() {
		var dayID int64
		var dayContent string
		var smID int64
		var smContent string
		if err := rows.Scan(&dayID, &dayContent, &smID, &smContent); err != nil {
			span.RecordError(err)
			return nil, fmt.Errorf("scan summary group row: %w", err)
		}

		idx, seen := index[dayID]
		if !seen {
			groups = append(groups, memory.SummaryGroup{DayID: dayID, Day: dayContent})
			idx = len(groups) - 1
			index[dayID] = idx
		}
		groups[idx].Summaries = append(groups[idx].Summaries, memory.NodeRef{ID: smID, Content: smContent})
	}
	if err := rows.Err(); err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("iterate summary group rows: %w", err)
	}

	span.SetAttributes(attribute.Int("db.group_count", len(groups)))
	return groups, nil
}

// ReplaceSummariesWithDigest writes a digest node under dayID and deletes the constituent summary nodes in a single transaction. FTS5 stays correct via the nodes_ai_summary (insert) and nodes_ad_summary (delete) triggers.
// If the insert fails the deletes never happen — summaries are never lost.
func (s *Store) ReplaceSummariesWithDigest(ctx context.Context, dayID int64, summaryIDs []int64, digest string) error {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.ReplaceSummariesWithDigest")
	defer span.End()

	if len(summaryIDs) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		span.RecordError(err)
		return fmt.Errorf("begin replace tx: %w", err)
	}
	defer tx.Rollback()

	// insert digest node — triggers nodes_ai_summary which indexes into FTS5
	res, err := tx.ExecContext(ctx,
		`INSERT INTO nodes (parent_id, type, content) VALUES (?, 'digest', ?)`,
		dayID, digest)
	if err != nil {
		span.RecordError(err)
		return fmt.Errorf("insert digest node: %w", err)
	}
	digestID, err := res.LastInsertId()
	if err != nil {
		span.RecordError(err)
		return fmt.Errorf("digest node id: %w", err)
	}

	// delete summaries — triggers nodes_ad_summary which removes from FTS5.
	// Build a parameterized IN clause manually since the driver doesn't support []int64 expansion.
	placeholders := make([]string, len(summaryIDs))
	args := make([]any, len(summaryIDs))
	for i, id := range summaryIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	deleteSQL := "DELETE FROM nodes WHERE id IN (" + strings.Join(placeholders, ",") + ")"
	if _, err := tx.ExecContext(ctx, deleteSQL, args...); err != nil {
		span.RecordError(err)
		return fmt.Errorf("delete summary nodes: %w", err)
	}

	if err := tx.Commit(); err != nil {
		span.RecordError(err)
		return fmt.Errorf("commit replace tx: %w", err)
	}

	span.SetAttributes(
		attribute.Int64("db.day_id", dayID),
		attribute.Int("db.summaries_replaced", len(summaryIDs)),
	)

	s.mu.RLock()
	emb, vidx := s.embedder, s.vectorIndex
	s.mu.RUnlock()
	if vidx != nil {
		go func(ids []int64) {
			delCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			for _, id := range ids {
				if err := vidx.Delete(delCtx, fmt.Sprintf("summary:%d", id)); err != nil {
					slog.Error("async summary vector delete failed", "summary_id", id, "error", err)
				}
			}
		}(summaryIDs)
	}

	// Async, best-effort embedding of the new digest, same non-blocking pattern as LogSemanticNode's summary embed goroutine. Without this the digest is FTS-only forever — the whole point of a digest is to still answer "what did I do that day" through the semantic half of HybridSearch.
	if emb != nil && vidx != nil && strings.TrimSpace(digest) != "" {
		go func(id int64, text string) {
			embedCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			vec, err := emb.Embed(embedCtx, "RETRIEVAL_DOCUMENT", text)
			if err != nil {
				slog.Error("async digest embed failed", "digest_id", id, "error", err)
				return
			}
			meta := map[string]string{
				"domain":     "", // digest nodes don't carry a domain today; key stays present so chromem's exact-match where doesn't drop this vector from domain-filtered searches
				"source":     "digest",
				"kind":       string(memory.KindPeriod),
				"created_at": time.Now().UTC().Format(time.RFC3339),
			}
			if err := vidx.Add(embedCtx, fmt.Sprintf("digest:%d", id), text, vec, meta); err != nil {
				slog.Error("async digest vector index add failed", "digest_id", id, "error", err)
			}
		}(digestID, digest)
	}
	return nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// LogSemanticNode implements the memory.Storage interface: it creates task nodes and summary leaf nodes in the temporal tree.
func (s *Store) LogSemanticNode(ctx context.Context, summary memory.TaskSummary) error {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "LogSemanticNode")
	defer span.End()

	s.mu.Lock()
	defer s.mu.Unlock()

	// Every summary writer funnels through here, so strip object replacement characters once at the door: window titles carry U+FFFC into the compiler's raw-activity fallback and occasionally into LLM summaries.
	summary.TaskName = memory.StripObjectChars(summary.TaskName)
	summary.Summary = memory.StripObjectChars(summary.Summary)

	// if new task, and no task id - create task node
	if !summary.SameTask || s.currentTaskID == 0 {
		taskID, err := s.ensureNode(ctx, s.currentParentID, "task", summary.TaskName)
		if err != nil {
			span.RecordError(err)
			return err
		}
		s.currentTaskID = taskID
	}

	// log summary as child to task node
	payload, _ := json.Marshal(summary)
	nodeID, err := s.ensureNode(ctx, s.currentTaskID, "summary", string(payload))
	if err != nil {
		span.RecordError(err)
		return err
	}

	// Domain: majority vote over episodes logged since the current task started (task.created_at), not a single Classify(app,title) call — a task can span many app switches, so "the domain of this task" is whichever domain dominated the episodes that fed it. Ties broken by domain string for determinism.
	var domain string
	if derr := s.db.QueryRowContext(ctx, `
		SELECT domain FROM episodes
		WHERE created_at >= (SELECT created_at FROM nodes WHERE id = ?)
		GROUP BY domain
		ORDER BY COUNT(*) DESC, domain
		LIMIT 1
	`, s.currentTaskID).Scan(&domain); derr != nil && derr != sql.ErrNoRows {
		span.RecordError(derr)
		// non-fatal: domain tagging is best-effort, the summary node itself is already committed above.
	}
	if domain != "" {
		if _, err := s.db.ExecContext(ctx, `UPDATE nodes SET domain = ? WHERE id = ?`, domain, nodeID); err != nil {
			span.RecordError(err)
		}
	}

	span.SetAttributes(
		attribute.String("db.task_name", summary.TaskName),
		attribute.Bool("db.same_task", summary.SameTask),
		attribute.String("db.summary_domain", domain),
	)

	// Async, best-effort embedding of the summary text (same non-blocking pattern as LogEpisode — see its doc comment for why this goroutine owns its own context rather than reusing ctx).
	// Snapshot under the write lock already held above — do NOT RLock again (sync.RWMutex isn't reentrant; nested RLock while holding Lock deadlocks).
	emb, vidx := s.embedder, s.vectorIndex
	if emb != nil && vidx != nil && strings.TrimSpace(summary.Summary) != "" {
		go func(id int64, text string, domain string) {
			embedCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			vec, err := emb.Embed(embedCtx, "RETRIEVAL_DOCUMENT", text)
			if err != nil {
				slog.Error("async summary embed failed", "node_id", id, "error", err)
				return
			}
			meta := map[string]string{
				"domain":     domain,
				"source":     "summary",
				"kind":       string(memory.KindPeriod),
				"created_at": time.Now().UTC().Format(time.RFC3339),
			}
			if err := vidx.Add(embedCtx, fmt.Sprintf("summary:%d", id), text, vec, meta); err != nil {
				slog.Error("async summary vector index add failed", "node_id", id, "error", err)
			}
		}(nodeID, summary.Summary, domain)
	}

	return nil
}

// DB exposes the underlying connection for test-only raw queries.
func (s *Store) DB() *sql.DB { return s.db }
