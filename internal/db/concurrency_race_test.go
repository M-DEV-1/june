package db_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"ora/internal/db"
)

// fakeEmbed returns a fixed-size vector so HybridSearch's vector path can run under -race without a network.
type fakeEmbed struct{}

func (fakeEmbed) Embed(ctx context.Context, task, text string) ([]float32, error) {
	v := make([]float32, 8)
	for i := range v {
		v[i] = float32(len(text)%7) * 0.1
	}
	return v, nil
}

type memIndex struct {
	mu   sync.Mutex
	docs map[string]struct {
		content string
		meta    map[string]string
		emb     []float32
	}
}

func newMemIndex() *memIndex {
	return &memIndex{docs: make(map[string]struct {
		content string
		meta    map[string]string
		emb     []float32
	})}
}

func (m *memIndex) Add(ctx context.Context, id, content string, embedding []float32, metadata map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	meta := make(map[string]string, len(metadata))
	for k, v := range metadata {
		meta[k] = v
	}
	emb := append([]float32(nil), embedding...)
	m.docs[id] = struct {
		content string
		meta    map[string]string
		emb     []float32
	}{content, meta, emb}
	return nil
}

func (m *memIndex) Search(ctx context.Context, queryEmbedding []float32, n int, where map[string]string) ([]db.Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []db.Result
	for id, d := range m.docs {
		if where != nil {
			skip := false
			for k, v := range where {
				if d.meta[k] != v {
					skip = true
					break
				}
			}
			if skip {
				continue
			}
		}
		out = append(out, db.Result{ID: id, Content: d.content, Metadata: d.meta, Similarity: 0.5})
		if len(out) >= n {
			break
		}
	}
	return out, nil
}

func (m *memIndex) Delete(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.docs, id)
	return nil
}

func (m *memIndex) IDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.docs))
	for id := range m.docs {
		ids = append(ids, id)
	}
	return ids
}


// TestConcurrentLogEpisodeAndHybridSearch hammers write + search under -race. Catches unlocked embedder field access and map races in the vector half.
func TestConcurrentLogEpisodeAndHybridSearch(t *testing.T) {
	// File-backed DB (not :memory:): each pool connection must see the same schema under concurrent LogEpisode + HybridSearch. Plain :memory: is per-connection in database/sql + modernc/sqlite.
	path := filepath.Join(t.TempDir(), "race.db")
	store, err := db.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// One connection avoids SQLITE_BUSY noise; -race still covers goroutine interleaving on embedder fields and the vector index.
	store.DB().SetMaxOpenConns(1)

	store.SetEmbedder(fakeEmbed{})
	store.SetVectorIndex(newMemIndex())

	ctx := context.Background()
	var wg sync.WaitGroup
	errCh := make(chan error, 64)

	// Writers: many LogEpisode (each spawns async embed).
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := store.LogEpisode(ctx, "Code", fmt.Sprintf("file%d.go", i),
				fmt.Sprintf("editing hybrid search path number %d with enough words to keep", i))
			if err != nil {
				errCh <- err
			}
		}(i)
	}

	// Readers: HybridSearch / RetrieveRelevant while embeds land.
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := store.HybridSearch(ctx, "hybrid search", "", 5); err != nil {
				errCh <- err
			}
			if _, err := store.RetrieveRelevant(ctx, "hybrid", 3); err != nil {
				errCh <- err
			}
		}()
	}

	// notes + summaries in parallel (summary embed path was unlocked before)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := store.LogNote(ctx, fmt.Sprintf("fact number %d about dark mode preference", i), "preference"); err != nil {
				errCh <- err
			}
		}(i)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("timeout waiting for concurrent ops")
	}

	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Errorf("concurrent op: %v", err)
		}
	}

	// Drain async embed goroutines spawned by LogEpisode/LogNote so -race doesn't observe them after the test returns.
	time.Sleep(200 * time.Millisecond)
}
