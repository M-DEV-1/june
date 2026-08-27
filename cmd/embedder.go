package cmd

import (
	"context"
	"fmt"

	"ora/internal/config"
	"ora/internal/embed"

	"google.golang.org/genai"
)

// newSharedGeminiEmbedder builds the genai-backed embedder used to wire db.Store's semantic half — same construction recipe the daemon and the client both need (daemon wires it directly alongside its own vector index; the client wires it locally too, with vector search itself going over IPC to the daemon instead — see httpVectorIndex).
func newSharedGeminiEmbedder(ctx context.Context, apiKey string) (*embed.GeminiEmbedder, error) {
	embedClient, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("init genai client for embeddings: %w", err)
	}
	return embed.NewGeminiEmbedder(embedClient.Models, config.EmbedModel, 3072), nil
}
