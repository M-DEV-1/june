package db

import (
	"context"
	"fmt"
	"go.opentelemetry.io/otel/attribute"
	"log/slog"
	"math"
	"ora/internal/memory"
	"ora/internal/obs"
	oratext "ora/internal/text"
	"os"
	"path/filepath"
	"strings"
	"time"
)

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
	// Prefer normalized content; if normalize emptied a non-empty raw capture of only chrome, fall back to raw so we never store an empty row as if it were a real observation. The fallback passes through StripObjectChars so a titleless capture of pure U+FFFC placeholders cannot smuggle uncleaned text into storage.
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
	// The uncapped variant: the same chrome-stripping, over the whole capture. chunkText below splits it into passages, which is what the 120-word cap used to prevent the need for — and what made a 96,061-character screen reach the index as roughly 700 characters.
	embedText := memory.NormalizeFull(obs.Context.App, obs.Context.Title, content).Document()
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

			meta := map[string]string{
				"domain":     string(domain),
				"source":     "episode",
				"kind":       string(memory.KindMoment),
				"created_at": time.Now().UTC().Format(time.RFC3339),
			}
			// One vector per passage, not per screen. A capture longer than the embedder's cap used to be represented by its first 4,000 runes and nothing else; a capture shorter than one chunk still produces exactly one, under the same id it always had.
			for i, chunk := range chunkText(text, chunkRunes, chunkOverlap) {
				vec, err := emb.Embed(embedCtx, "RETRIEVAL_DOCUMENT", chunk)
				if err != nil {
					slog.Error("async episode embed failed", "episode_id", id, "chunk", i, "error", err)
					continue
				}
				if err := vidx.Add(embedCtx, chunkVectorID("episode", id, i), chunk, vec, meta); err != nil {
					slog.Error("async episode vector index add failed", "episode_id", id, "chunk", i, "error", err)
				}
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
		out = append(out, fmt.Sprintf("[episode] %s", oratext.Runes(h.Content, maxEpisodeExcerpt)))
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

// maxExtraFrames caps how many non-primary monitors get a frame on disk, because the suffix letters run b..z. Nobody has 26 screens, and a bad caller must not be able to fill the frames dir.
const maxExtraFrames = 25

// extraFrameName is the file name for the i-th extra monitor of an episode: 0 -> "{id}-b.jpg", 1 -> "{id}-c.jpg", and so on. The primary monitor stays "{id}.jpg", so the whole set for an episode is derivable from its id alone and needs no column of its own.
func extraFrameName(id int64, i int) string {
	return fmt.Sprintf("%d-%c.jpg", id, 'b'+rune(i))
}

// writeEpisodeJPEG stores the primary monitor's frame under framesDir/{id}.jpg and each extra monitor's frame beside it as {id}-b.jpg, {id}-c.jpg, then returns the relative path "frames/{id}.jpg" for the primary. Empty if there is no primary image or no frames dir (:memory:).
// Extras are only written when the primary was written: image_path is what the aging queries look for, so an extra with no primary would never be found again and never be reclaimed.
func (s *Store) writeEpisodeJPEG(id int64, jpeg []byte, extras [][]byte) string {
	if len(jpeg) == 0 || s.framesDir == "" {
		return ""
	}
	if err := os.MkdirAll(s.framesDir, 0700); err != nil {
		slog.Error("create frames dir failed", "dir", s.framesDir, "error", err)
		return ""
	}
	abs := filepath.Join(s.framesDir, fmt.Sprintf("%d.jpg", id))
	if err := os.WriteFile(abs, jpeg, 0600); err != nil {
		slog.Error("write episode jpeg failed", "path", abs, "error", err)
		return ""
	}
	for i, extra := range extras {
		if len(extra) == 0 || i >= maxExtraFrames {
			break
		}
		path := filepath.Join(s.framesDir, extraFrameName(id, i))
		if err := os.WriteFile(path, extra, 0600); err != nil {
			slog.Error("write episode extra monitor jpeg failed", "path", path, "error", err)
			break
		}
	}
	return filepath.ToSlash(filepath.Join("frames", fmt.Sprintf("%d.jpg", id)))
}

// EpisodeExtraImages returns the relative paths of the frames captured from the episode's other monitors, in capture order, or nil when the capture was single-screen or the frames have been aged away. Paths are relative to the store's directory, the same shape as Episode.ImagePath.
func (s *Store) EpisodeExtraImages(id int64) []string {
	if s.framesDir == "" {
		return nil
	}
	var out []string
	for i := 0; i < maxExtraFrames; i++ {
		name := extraFrameName(id, i)
		if _, err := os.Stat(filepath.Join(s.framesDir, name)); err != nil {
			break
		}
		out = append(out, filepath.ToSlash(filepath.Join("frames", name)))
	}
	return out
}

// removeEpisodeJPEG deletes every monitor's frame for the episode. Every path that ages or prunes an episode goes through here, so an extra monitor's frame can never outlive the primary it was captured with.
func (s *Store) removeEpisodeJPEG(id int64) {
	if s.framesDir == "" {
		return
	}
	_ = os.Remove(filepath.Join(s.framesDir, fmt.Sprintf("%d.jpg", id)))
	for i := 0; i < maxExtraFrames; i++ {
		if err := os.Remove(filepath.Join(s.framesDir, extraFrameName(id, i))); err != nil {
			break
		}
	}
}

// DistinctTitles returns the distinct window titles the tracker has recorded, most recently seen first.
//
// Input: how many to return. Output: one entry per distinct title.
//
// This is what tells an identifying word in a window title from the furniture around it. A browser writes its own state into the title bar — the app's name, "Microphone recording", "High memory usage" — and those phrases recur across hundreds of unrelated titles, while the words naming an actual meeting appear in one or two. Counting titles is what separates them, and it needs no list of which words a browser uses.
func (s *Store) DistinctTitles(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT title FROM episodes
		WHERE title != ''
		GROUP BY title
		ORDER BY MAX(created_at) DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var titles []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		titles = append(titles, t)
	}
	return titles, rows.Err()
}
