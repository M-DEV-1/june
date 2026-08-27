package vector

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	chromem "github.com/philippgille/chromem-go"
)

// ChromemIndex is the chromem-go-backed Index, with oldest-first eviction to enforce maxDocs. chromem-go has no query for "all docs by metadata", so createdAt is tracked in a sidecar JSON file that survives a restart.
type ChromemIndex struct {
	mu      sync.Mutex
	maxDocs int

	col         *chromem.Collection
	sidecarPath string
	createdAt   map[string]time.Time // id -> createdAt, for oldest-first eviction
}

// loadSidecar reads the sidecar file at path. A missing file just means a fresh index, so that's an empty map, not an error.
func loadSidecar(path string) (map[string]time.Time, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]time.Time), nil
		}
		return nil, fmt.Errorf("read sidecar: %w", err)
	}
	var out map[string]time.Time
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("unmarshal sidecar: %w", err)
	}
	if out == nil {
		out = make(map[string]time.Time)
	}
	return out, nil
}

// persistSidecar rewrites the sidecar JSON file in full with the current
// createdAt map.
func (c *ChromemIndex) persistSidecar() error {
	data, err := json.Marshal(c.createdAt)
	if err != nil {
		return fmt.Errorf("marshal sidecar: %w", err)
	}
	if err := os.WriteFile(c.sidecarPath, data, 0644); err != nil {
		return fmt.Errorf("write sidecar: %w", err)
	}
	return nil
}

// NewChromemIndex opens or creates a persistent, gzip-compressed chromem-go collection at dbPath/collectionName, enforcing maxDocs via oldest-first eviction on Add.
func NewChromemIndex(dbPath, collectionName string, maxDocs int) (*ChromemIndex, error) {
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
	createdAt, err := loadSidecar(sidecarPath)
	if err != nil {
		return nil, fmt.Errorf("load chromem sidecar: %w", err)
	}

	return &ChromemIndex{
		maxDocs:     maxDocs,
		col:         col,
		sidecarPath: sidecarPath,
		createdAt:   createdAt,
	}, nil
}

// Add inserts or overwrites a document, then evicts the oldest entries by createdAt if that pushes past maxDocs.
func (c *ChromemIndex) Add(ctx context.Context, id, content string, embedding []float32, metadata map[string]string) error {
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
