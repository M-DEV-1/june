package vector

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"ora/internal/util"

	chromem "github.com/philippgille/chromem-go"
)

// ChromemIndex is the chromem-go-backed Index, with oldest-first eviction to enforce maxDocs. chromem-go has no query for "all docs by metadata", so createdAt is tracked in a sidecar JSON file that survives a restart.
type ChromemIndex struct {
	mu      sync.Mutex
	maxDocs int
	dim     int

	col         *chromem.Collection
	sidecarPath string
	createdAt   map[string]time.Time // id -> createdAt, for oldest-first eviction
}

// collectionName is the single chromem collection ORA keeps its vectors in.
const collectionName = "memory"

// loadSidecar reads the sidecar file at path. A missing, unreadable, or corrupt file (e.g. truncated JSON left by a crash mid-write, before persistSidecar wrote atomically) just means starting from an empty map rather than failing — the sidecar only holds recoverable createdAt bookkeeping for eviction ordering, not the vectors themselves, so refusing to construct the index over it would silently and permanently disable semantic search instead (see FINDING 5: the caller only logs a warning and leaves the index nil, with nothing to repair the file afterward).
func loadSidecar(path string) map[string]time.Time {
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("chromem sidecar unreadable, starting from an empty createdAt map", "path", path, "error", err)
		}
		return make(map[string]time.Time)
	}
	var out map[string]time.Time
	if err := json.Unmarshal(data, &out); err != nil {
		slog.Warn("chromem sidecar corrupt, starting from an empty createdAt map", "path", path, "error", err)
		return make(map[string]time.Time)
	}
	if out == nil {
		out = make(map[string]time.Time)
	}
	return out
}

// persistSidecar rewrites the sidecar JSON file with the current createdAt map, atomically: written to a temp file in the same directory then renamed over the target, so a crash or power loss mid-write can never leave loadSidecar a truncated file to choke on.
// ponytail: this is a full-map marshal+write under c.mu on every Add — O(n) in doc count, fine at the deck's 10k-doc cap; upgrade to an append-only log or drop the sidecar for a real KV store if profiling ever shows this mattering.
func (c *ChromemIndex) persistSidecar() error {
	data, err := json.Marshal(c.createdAt)
	if err != nil {
		return fmt.Errorf("marshal sidecar: %w", err)
	}
	if err := util.WriteFileAtomic(c.sidecarPath, data, 0o600); err != nil {
		return fmt.Errorf("write sidecar: %w", err)
	}
	return nil
}

// NewChromemIndex opens or creates a persistent, gzip-compressed chromem-go collection at dbPath, holding vectors of dim dimensions and enforcing maxDocs via oldest-first eviction on Add.
// dim is what the current embedding model produces. chromem refuses any query that mixes vector widths, so a collection left behind by a different model would make every search fail; such a collection is dropped and rebuilt here instead (the reconcile sweep refills it) rather than left to degrade search silently and permanently. The sidecar's createdAt bookkeeping is likewise rebuilt from the collection whenever it has drifted, since the reconcile sweep reads the id list out of it.
func NewChromemIndex(dbPath string, dim, maxDocs int) (*ChromemIndex, error) {
	if err := os.MkdirAll(dbPath, 0755); err != nil {
		return nil, fmt.Errorf("create chromem db dir: %w", err)
	}

	db, err := chromem.NewPersistentDB(dbPath, true)
	if err != nil {
		return nil, fmt.Errorf("open chromem persistent db: %w", err)
	}

	col, err := db.GetOrCreateCollection(collectionName, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("get or create chromem collection: %w", err)
	}

	sidecarPath := filepath.Join(dbPath, collectionName+"_meta.json")
	c := &ChromemIndex{
		maxDocs:     maxDocs,
		dim:         dim,
		col:         col,
		sidecarPath: sidecarPath,
		createdAt:   loadSidecar(sidecarPath),
	}

	ids, err := c.collectionIDs(context.Background())
	if err != nil {
		slog.Warn("chromem collection cannot be queried at the current embedding width, dropping it so the reconcile sweep can rebuild it", "docs", col.Count(), "dim", dim, "error", err)
		if err := db.DeleteCollection(collectionName); err != nil {
			return nil, fmt.Errorf("drop mismatched chromem collection: %w", err)
		}
		if c.col, err = db.GetOrCreateCollection(collectionName, nil, nil); err != nil {
			return nil, fmt.Errorf("recreate chromem collection: %w", err)
		}
		ids = nil
	}
	c.reconcileSidecar(ids)

	return c, nil
}

// collectionIDs returns the id of every document in the collection, or an error when the collection's vectors are not dim wide. chromem has no "list documents" call, so this rides a nearest-neighbour query over the whole collection — which is also what makes it a width check, since chromem compares the probe against every stored vector and refuses on a length mismatch.
func (c *ChromemIndex) collectionIDs(ctx context.Context) ([]string, error) {
	n := c.col.Count()
	if n == 0 {
		return nil, nil
	}
	probe := make([]float32, c.dim)
	probe[0] = 1
	results, err := c.col.QueryEmbedding(ctx, probe, n, nil, nil)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(results))
	for _, r := range results {
		ids = append(ids, r.ID)
	}
	return ids, nil
}

// reconcileSidecar makes the createdAt map name exactly the documents the collection holds: ids the sidecar lost (a crash before the atomic rewrite, a deleted file) come back dated now, and ids for documents that are no longer there are dropped. Called only at open, with no other reference to the index yet, so it takes no lock. A no-op when the two already agree.
func (c *ChromemIndex) reconcileSidecar(ids []string) {
	inCollection := make(map[string]bool, len(ids))
	changed := false
	for _, id := range ids {
		inCollection[id] = true
		if _, ok := c.createdAt[id]; !ok {
			c.createdAt[id] = time.Now()
			changed = true
		}
	}
	for id := range c.createdAt {
		if !inCollection[id] {
			delete(c.createdAt, id)
			changed = true
		}
	}
	if !changed {
		return
	}
	slog.Info("rebuilt the chromem sidecar from the collection", "docs", len(ids))
	if err := c.persistSidecar(); err != nil {
		slog.Warn("could not persist the rebuilt chromem sidecar", "error", err)
	}
}

// Add inserts or overwrites a document, then evicts the oldest entries by createdAt if that pushes past maxDocs.
func (c *ChromemIndex) Add(ctx context.Context, id, content string, embedding []float32, metadata map[string]string) error {
	// One wrong-width vector poisons the whole collection: the width probe at open would then drop and rebuild everything, so a mismatch is rejected at the door instead.
	if c.dim > 0 && len(embedding) != c.dim {
		return fmt.Errorf("chromem add %s: embedding has %d dimensions, index expects %d", id, len(embedding), c.dim)
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.col.Add(ctx, []string{id}, [][]float32{embedding}, []map[string]string{metadata}, []string{content}); err != nil {
		return fmt.Errorf("chromem add: %w", err)
	}

	c.createdAt[id] = time.Now()
	if err := c.persistSidecar(); err != nil {
		return err
	}

	return c.evictOverCap(ctx)
}

// evictOverCap removes oldest entries (by c.createdAt) until count is back at or under maxDocs. Caller must hold mu.
func (c *ChromemIndex) evictOverCap(ctx context.Context) error {
	if c.maxDocs <= 0 || len(c.createdAt) <= c.maxDocs {
		return nil
	}

	type idAge struct {
		id  string
		age time.Time
	}
	ordered := make([]idAge, 0, len(c.createdAt))
	for id, t := range c.createdAt {
		ordered = append(ordered, idAge{id, t})
	}
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].age.Before(ordered[j].age)
	})

	excess := len(c.createdAt) - c.maxDocs
	for i := 0; i < excess; i++ {
		id := ordered[i].id
		if err := c.col.Delete(ctx, nil, nil, id); err != nil {
			return fmt.Errorf("evict oldest %q: %w", id, err)
		}
		delete(c.createdAt, id)
	}
	return c.persistSidecar()
}

// Search runs a nearest-neighbor query, capped at n and filtered by exact-match where.
//
// Holds c.mu for the whole call — chromem-go's document map isn't safe for concurrent Add+Search, and LogEpisode's embed goroutines call Add while HybridSearch calls Search.
func (c *ChromemIndex) Search(ctx context.Context, queryEmbedding []float32, n int, where map[string]string) ([]Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// chromem errors if n exceeds the doc count, so clamp instead of making callers track the current size.
	if count := c.col.Count(); n > count {
		n = count
	}
	if n <= 0 {
		return nil, nil
	}

	results, err := c.col.QueryEmbedding(ctx, queryEmbedding, n, where, nil)
	if err != nil {
		return nil, fmt.Errorf("chromem query: %w", err)
	}

	out := make([]Result, len(results))
	for i, r := range results {
		// clone metadata so callers don't alias chromem's internal map after we unlock.
		meta := make(map[string]string, len(r.Metadata))
		for k, v := range r.Metadata {
			meta[k] = v
		}
		out[i] = Result{
			ID:         r.ID,
			Content:    r.Content,
			Metadata:   meta,
			Similarity: r.Similarity,
		}
	}
	return out, nil
}

// Delete removes id from both the chromem collection and the sidecar map.
func (c *ChromemIndex) Delete(ctx context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.col.Delete(ctx, nil, nil, id); err != nil {
		return fmt.Errorf("chromem delete: %w", err)
	}
	delete(c.createdAt, id)
	return c.persistSidecar()
}

// Count returns the doc count from chromem — the source of truth Search/Delete operate against.
func (c *ChromemIndex) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.col.Count()
}

// IDs returns every doc id currently in the index — the sidecar map (already the source of truth for eviction) doubles as the id list, so this is a simple key dump rather than a chromem query. Used by the reconciliation sweep (internal/db) to find vectors whose backing SQL row is gone or that are missing a vector entirely.
func (c *ChromemIndex) IDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	ids := make([]string, 0, len(c.createdAt))
	for id := range c.createdAt {
		ids = append(ids, id)
	}
	return ids
}
