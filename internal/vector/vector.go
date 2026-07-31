// Package vector is the nearest-neighbor vector store used by hybrid retrieval (internal/db's HybridSearch). ChromemIndex is the only implementation, backed by chromem-go.
package vector

import "context"

// Result is one nearest-neighbor hit.
type Result struct {
	ID         string
	Content    string
	Metadata   map[string]string
	Similarity float32
}

// Index is a nearest-neighbor vector store, swappable — ChromemIndex is the only implementation today.
type Index interface {
	Add(ctx context.Context, id, content string, embedding []float32, metadata map[string]string) error
	Search(ctx context.Context, queryEmbedding []float32, n int, where map[string]string) ([]Result, error)
	Delete(ctx context.Context, id string) error
	Count() int
}
