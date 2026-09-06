package db

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
)

// Migration represents a single versioned database migration, analogous to Prisma or Drizzle schema migrations.
type Migration struct {
	Version int
	Name    string
	Up      func(ctx context.Context, tx *sql.Tx) error
}

// ensureColumnInTx checks via PRAGMA table_info whether column exists in table, and runs ALTER TABLE ADD COLUMN if missing.
// modernc.org/sqlite doesn't support ALTER TABLE ADD COLUMN IF NOT EXISTS (syntax error, not a no-op), so table_info inspects column names first.
func ensureColumnInTx(ctx context.Context, tx *sql.Tx, table, column, decl string) error {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return fmt.Errorf("read columns for %s: %w", table, err)
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dfltValue sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
			return fmt.Errorf("scan column for %s: %w", table, err)
		}
		if strings.EqualFold(name, column) {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate columns for %s: %w", table, err)
	}

	_, err = tx.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, decl))
	if err != nil {
		return fmt.Errorf("add column %s.%s: %w", table, column, err)
	}
	return nil
}

// schemaMigrationsTable ensures the version tracker table exists before running migrations.
const schemaMigrationsTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
	version INTEGER PRIMARY KEY,
	name TEXT NOT NULL,
	applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
`

// migrations is the ordered ledger of database migrations.
var migrations = []Migration{
	{
		Version: 1,
		Name:    "0001_initial_schema",
		Up: func(ctx context.Context, tx *sql.Tx) error {
			ddl := `
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
	-- Two sorts of kind are left out, both bare markers rewritten on a schedule rather than anything Ora wrote: TaskNoticeWatermarkKind, whose content is a note id the proactive loop rewrites on most ticks (see diary.go), and every JobMarkerKindPrefix kind, whose content is the RFC 3339 moment one metered background job last ran (see cmd/daemon.go).
	-- The insert is written as INSERT ... SELECT ... WHERE rather than a trigger-level WHEN so the update trigger's DELETE still runs for every kind, which is what clears a marker row an older database had already mirrored.
	-- Dropped first so a database created before the watermark was excluded picks up the new bodies.
	DROP TRIGGER IF EXISTS diary_ai;
	DROP TRIGGER IF EXISTS diary_au;
	CREATE TRIGGER IF NOT EXISTS diary_ai AFTER INSERT ON diary BEGIN
		INSERT INTO memory_fts(content, source, ref_id)
			SELECT NEW.content, 'diary', NEW.id WHERE NEW.kind <> '` + TaskNoticeWatermarkKind + `' AND NEW.kind NOT LIKE '` + JobMarkerKindPrefix + `%';
	END;
	CREATE TRIGGER IF NOT EXISTS diary_ad AFTER DELETE ON diary BEGIN
		DELETE FROM memory_fts WHERE source='diary' AND ref_id = OLD.id;
	END;
	CREATE TRIGGER IF NOT EXISTS diary_au AFTER UPDATE ON diary BEGIN
		DELETE FROM memory_fts WHERE source='diary' AND ref_id = OLD.id;
		INSERT INTO memory_fts(content, source, ref_id)
			SELECT NEW.content, 'diary', NEW.id WHERE NEW.kind <> '` + TaskNoticeWatermarkKind + `' AND NEW.kind NOT LIKE '` + JobMarkerKindPrefix + `%';
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

	CREATE TABLE IF NOT EXISTS act_run_vectors (
		run_id INTEGER PRIMARY KEY REFERENCES act_runs(id) ON DELETE CASCADE,
		vec BLOB NOT NULL
	);

	CREATE TABLE IF NOT EXISTS token_use (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		provider TEXT NOT NULL,
		model TEXT NOT NULL DEFAULT '',
		channel TEXT NOT NULL DEFAULT '',
		input_tokens INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0,
		total_tokens INTEGER NOT NULL DEFAULT 0,
		cached_tokens INTEGER NOT NULL DEFAULT 0,
		rounds INTEGER NOT NULL DEFAULT 0,
		duration_ms INTEGER NOT NULL DEFAULT 0,
		question TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_token_use_created_at ON token_use(created_at, provider);

	CREATE TABLE IF NOT EXISTS tally (
		day TEXT NOT NULL,
		provider TEXT NOT NULL,
		calls INTEGER NOT NULL DEFAULT 0,
		failures INTEGER NOT NULL DEFAULT 0,
		total_ms INTEGER NOT NULL DEFAULT 0,
		prompt_chars INTEGER NOT NULL DEFAULT 0,
		reply_chars INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (day, provider)
	);
`
			_, err := tx.ExecContext(ctx, ddl)
			return err
		},
	},
	{
		Version: 2,
		Name:    "0002_domain_and_activity_columns",
		Up: func(ctx context.Context, tx *sql.Tx) error {
			if err := ensureColumnInTx(ctx, tx, "nodes", "domain", "TEXT NOT NULL DEFAULT ''"); err != nil {
				return err
			}
			if err := ensureColumnInTx(ctx, tx, "episodes", "domain", "TEXT NOT NULL DEFAULT ''"); err != nil {
				return err
			}
			if err := ensureColumnInTx(ctx, tx, "episodes", "user_activity", "TEXT NOT NULL DEFAULT ''"); err != nil {
				return err
			}
			if err := ensureColumnInTx(ctx, tx, "episodes", "visible_text", "TEXT NOT NULL DEFAULT ''"); err != nil {
				return err
			}
			return ensureColumnInTx(ctx, tx, "episodes", "image_path", "TEXT NOT NULL DEFAULT ''")
		},
	},
	{
		Version: 3,
		Name:    "0003_add_tally_char_counts",
		Up: func(ctx context.Context, tx *sql.Tx) error {
			if err := ensureColumnInTx(ctx, tx, "tally", "prompt_chars", "INTEGER NOT NULL DEFAULT 0"); err != nil {
				return err
			}
			return ensureColumnInTx(ctx, tx, "tally", "reply_chars", "INTEGER NOT NULL DEFAULT 0")
		},
	},
	{
		Version: 4,
		Name:    "0004_add_token_use_cache_and_rounds",
		Up: func(ctx context.Context, tx *sql.Tx) error {
			if err := ensureColumnInTx(ctx, tx, "token_use", "cached_tokens", "INTEGER NOT NULL DEFAULT 0"); err != nil {
				return err
			}
			return ensureColumnInTx(ctx, tx, "token_use", "rounds", "INTEGER NOT NULL DEFAULT 0")
		},
	},
	{
		Version: 5,
		Name:    "0005_add_act_job_checkpoints",
		Up: func(ctx context.Context, tx *sql.Tx) error {
			cols := []struct{ name, decl string }{
				{"job_id", "TEXT NOT NULL DEFAULT ''"},
				{"job_state", "TEXT NOT NULL DEFAULT ''"},
				{"job_json", "TEXT NOT NULL DEFAULT ''"},
			}
			for _, col := range cols {
				if err := ensureColumnInTx(ctx, tx, "act_runs", col.name, col.decl); err != nil {
					return err
				}
			}
			return nil
		},
	},
	{
		Version: 6,
		Name:    "0006_add_diary_parent_and_notes_owner",
		Up: func(ctx context.Context, tx *sql.Tx) error {
			if err := ensureColumnInTx(ctx, tx, "diary", "parent_id", "INTEGER"); err != nil {
				return err
			}
			return ensureColumnInTx(ctx, tx, "notes", "owner_class", "TEXT NOT NULL DEFAULT ''")
		},
	},
	{
		Version: 7,
		Name:    "0007_cleanup_fts_content_and_markers",
		Up: func(ctx context.Context, tx *sql.Tx) error {
			// Migration for DBs written before nodes_ai_summary extracted $.summary: their summary rows still hold the raw marshalled TaskSummary, so the JSON keys stay searchable until the text is rewritten. Idempotent — a rewritten row is no longer JSON, so the guard skips it on every later run.
			if _, err := tx.ExecContext(ctx, `
				UPDATE memory_fts SET content = json_extract(content, '$.summary')
				WHERE source IN ('summary','digest')
					AND json_valid(content)
					AND NULLIF(json_extract(content, '$.summary'), '') IS NOT NULL`); err != nil {
				return fmt.Errorf("rebuild summary fts content: %w", err)
			}

			// Migration for DBs written while the diary triggers still mirrored every kind: the task-notice watermark's bare note id and each background job's bare last-run timestamp are sitting in the search index as if they were memories. Idempotent — there is nothing left to delete on every later run.
			if _, err := tx.ExecContext(ctx, `
				DELETE FROM memory_fts
				WHERE source = 'diary'
				  AND ref_id IN (SELECT id FROM diary WHERE kind = ? OR kind LIKE ?)`, TaskNoticeWatermarkKind, JobMarkerKindPrefix+"%"); err != nil {
				return fmt.Errorf("clear the diary's bare markers from the search index: %w", err)
			}
			return nil
		},
	},
}

// runMigrations applies all pending schema migrations sequentially in transactions.
func (s *Store) runMigrations(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schemaMigrationsTable); err != nil {
		return fmt.Errorf("create schema_migrations table: %w", err)
	}

	applied := make(map[int]bool)
	rows, err := s.db.QueryContext(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return fmt.Errorf("query applied migrations: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return fmt.Errorf("scan migration version: %w", err)
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate migration versions: %w", err)
	}

	for _, m := range migrations {
		if applied[m.Version] {
			continue
		}

		slog.Debug("applying database migration", "version", m.Version, "name", m.Name)
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %s (%d): %w", m.Name, m.Version, err)
		}

		if err := m.Up(ctx, tx); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %s (%d): %w", m.Name, m.Version, err)
		}

		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations (version, name) VALUES (?, ?)", m.Version, m.Name); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %s (%d): %w", m.Name, m.Version, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s (%d): %w", m.Name, m.Version, err)
		}
		slog.Info("applied database migration", "version", m.Version, "name", m.Name)
	}

	return nil
}
