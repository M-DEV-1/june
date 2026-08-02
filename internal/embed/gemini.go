package embed

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/genai"
)

// embedContentCaller is the slice of genai.Models GeminiEmbedder needs, so tests can fake it instead of hitting a live client. EmbedContent is a value-type method on Models, so *genai.Client's Models field satisfies this interface with no adapter needed.
type embedContentCaller interface {
	EmbedContent(ctx context.Context, model string, contents []*genai.Content, config *genai.EmbedContentConfig) (*genai.EmbedContentResponse, error)
}

// GeminiEmbedder is the Embedder backed by the gemini-embedding-2-preview model via genai.Models.EmbedContent.
type GeminiEmbedder struct {
	caller embedContentCaller
	model  string
	dim    int32
}

// NewGeminiEmbedder wires a GeminiEmbedder to caller (a real *genai.Client's Models field, or a fake in tests), the model name, and the requested OutputDimensionality.
func NewGeminiEmbedder(caller embedContentCaller, model string, dim int32) *GeminiEmbedder {
	return &GeminiEmbedder{caller: caller, model: model, dim: dim}
}

// Embed sends text to the embedding model for the given TaskType and returns the resulting vector. Errors instead of calling the API if text is empty or whitespace-only, and errors instead of panicking if the response contains no embeddings.
func (g *GeminiEmbedder) Embed(ctx context.Context, task TaskType, text string) ([]float32, error) {
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("embed: text is empty/whitespace-only")
	}

	content := genai.NewContentFromText(text, "")
	resp, err := g.caller.EmbedContent(ctx, g.model, []*genai.Content{content}, &genai.EmbedContentConfig{
		TaskType:             string(task),
		OutputDimensionality: &g.dim,
	})
	if err != nil {
		return nil, fmt.Errorf("embed content: %w", err)
	}
	if len(resp.Embeddings) == 0 {
		return nil, fmt.Errorf("embed: response contained zero embeddings")
	}
	return resp.Embeddings[0].Values, nil
}
