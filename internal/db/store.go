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

	// framesDir is june-db/frames next to the sqlite file. Empty for :memory: stores — vision JPEGs are skipped.
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
	// WAL lets multiple connections read/write concurrently (daemon LogEpisode + tool HybridSearch); busy_timeout makes them wait instead of erroring SQLITE_BUSY immediately. 30 s rather than 5 s because a commit on Windows waits on a slow disk flush, and under several writers one of them waited past 5 s and lost its write.
	// foreign_keys is off by default in SQLite and is a property of a connection, not of the database, so it has to be in the DSN: the driver replays every _pragma here on each connection the pool opens, which a one-off Exec after sql.Open would not. Without it every ON DELETE CASCADE below is dead text — deleting a conversation left its turns behind, deleting an episode or a thread left dangling rows in episode_threads, and a turn could be written against a conversation id that names nothing. See foreign_keys_test.go.
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(30000)&_pragma=foreign_keys(1)"

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

// JobMarkerKindPrefix is what every diary kind recording a metered background job's last run starts with, the whole kind being this plus the job's name (cmd/daemon.go's jobMarkerKind is the only writer). It lives here because the diary FTS triggers below name it: the content of such a row is a bare RFC 3339 timestamp rewritten every time that job runs, so like the task-notice watermark it is deliberately never mirrored into memory_fts and never comes back from a search as if it were something June wrote.
const JobMarkerKindPrefix = "job-last-run:"

func (s *Store) createSchema() error {
	return s.runMigrations(context.Background())
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
