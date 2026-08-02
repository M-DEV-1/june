// Package embed turns text into vectors for the hybrid retrieval layer
// (internal/db's HybridSearch) and the vector index (internal/vector).
package embed

import "context"

// TaskType selects which of Gemini's embedding task modes to use.
type TaskType string

const (
	TaskRetrievalDocument TaskType = "RETRIEVAL_DOCUMENT"
	TaskRetrievalQuery    TaskType = "RETRIEVAL_QUERY"
)

// Embedder turns text into a vector. TaskType is asymmetric on purpose — RETRIEVAL_DOCUMENT on write, RETRIEVAL_QUERY on search — Gemini's docs call this a real recall difference, not a cosmetic label.
type Embedder interface {
	Embed(ctx context.Context, task TaskType, text string) ([]float32, error)
}
