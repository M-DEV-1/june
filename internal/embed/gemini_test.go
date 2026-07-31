package embed

// Lives in package embed (not embed_test) since embedContentCaller is unexported and the fake below implements it directly.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/genai"
)

// fakeEmbedContentCaller is a controllable stand-in for genai.Models, avoiding any real network call.
type fakeEmbedContentCaller struct {
	// response is returned verbatim on a successful call.
	response *genai.EmbedContentResponse
	// err, if non-nil, is returned instead of response.
	err error

	// captured call args, for assertions.
	called           bool
	capturedModel    string
	capturedContents []*genai.Content
	capturedConfig   *genai.EmbedContentConfig
}

func (f *fakeEmbedContentCaller) EmbedContent(ctx context.Context, model string, contents []*genai.Content, config *genai.EmbedContentConfig) (*genai.EmbedContentResponse, error) {
	f.called = true
	f.capturedModel = model
	f.capturedContents = contents
	f.capturedConfig = config
	if f.err != nil {
		return nil, f.err
	}
	return f.response, nil
}

func vectorResponse(values ...float32) *genai.EmbedContentResponse {
	return &genai.EmbedContentResponse{
		Embeddings: []*genai.ContentEmbedding{
			{Values: values},
		},
	}
}

// TestGeminiEmbedder_Embed_CallsCallerWithExpectedArgs verifies Embed builds one *genai.Content from the text, targets the configured model, and requests the configured OutputDimensionality.
func TestGeminiEmbedder_Embed_CallsCallerWithExpectedArgs(t *testing.T) {
	fake := &fakeEmbedContentCaller{response: vectorResponse(0.1, 0.2, 0.3)}
	g := NewGeminiEmbedder(fake, "gemini-embedding-2-preview", 768)

	if _, err := g.Embed(context.Background(), TaskRetrievalDocument, "the user likes go"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !fake.called {
		t.Fatalf("expected caller.EmbedContent to be called")
	}
	if fake.capturedModel != "gemini-embedding-2-preview" {
		t.Errorf("model = %q, want %q", fake.capturedModel, "gemini-embedding-2-preview")
	}
	if len(fake.capturedContents) != 1 {
		t.Fatalf("expected exactly one Content, got %d", len(fake.capturedContents))
	}
	got := fake.capturedContents[0]
	if len(got.Parts) != 1 || got.Parts[0].Text != "the user likes go" {
		t.Errorf("content = %+v, want single part with text %q", got, "the user likes go")
	}
	if fake.capturedConfig == nil {
		t.Fatalf("expected a non-nil EmbedContentConfig")
	}
	if fake.capturedConfig.OutputDimensionality == nil || *fake.capturedConfig.OutputDimensionality != 768 {
		t.Errorf("OutputDimensionality = %v, want 768", fake.capturedConfig.OutputDimensionality)
	}
}

// TestGeminiEmbedder_Embed_TaskTypeRetrievalDocumentPassedVerbatim verifies RETRIEVAL_DOCUMENT reaches the config unmodified — the write-side task type that matters for recall quality.
func TestGeminiEmbedder_Embed_TaskTypeRetrievalDocumentPassedVerbatim(t *testing.T) {
	fake := &fakeEmbedContentCaller{response: vectorResponse(1)}
	g := NewGeminiEmbedder(fake, "gemini-embedding-2-preview", 768)

	if _, err := g.Embed(context.Background(), TaskRetrievalDocument, "some document text"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.capturedConfig.TaskType != string(TaskRetrievalDocument) {
		t.Errorf("TaskType = %q, want %q", fake.capturedConfig.TaskType, TaskRetrievalDocument)
	}
}

// TestGeminiEmbedder_Embed_TaskTypeRetrievalQueryPassedVerbatim is the read-side analogue: a search-time Embed call must request RETRIEVAL_QUERY, not RETRIEVAL_DOCUMENT.
func TestGeminiEmbedder_Embed_TaskTypeRetrievalQueryPassedVerbatim(t *testing.T) {
	fake := &fakeEmbedContentCaller{response: vectorResponse(1)}
	g := NewGeminiEmbedder(fake, "gemini-embedding-2-preview", 768)

	if _, err := g.Embed(context.Background(), TaskRetrievalQuery, "some search query"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.capturedConfig.TaskType != string(TaskRetrievalQuery) {
		t.Errorf("TaskType = %q, want %q", fake.capturedConfig.TaskType, TaskRetrievalQuery)
	}
}

// TestGeminiEmbedder_Embed_ReturnsValuesOnSuccess verifies the returned vector is exactly resp.Embeddings[0].Values.
func TestGeminiEmbedder_Embed_ReturnsValuesOnSuccess(t *testing.T) {
	want := []float32{0.5, -0.25, 1.0, 3072.0}
	fake := &fakeEmbedContentCaller{response: vectorResponse(want...)}
	g := NewGeminiEmbedder(fake, "gemini-embedding-2-preview", 4)

	got, err := g.Embed(context.Background(), TaskRetrievalQuery, "hello world")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("len(got) = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestGeminiEmbedder_Embed_EmptyTextErrorsWithoutCallingAPI verifies an empty string fails fast — no wasted API call.
func TestGeminiEmbedder_Embed_EmptyTextErrorsWithoutCallingAPI(t *testing.T) {
	fake := &fakeEmbedContentCaller{response: vectorResponse(1)}
	g := NewGeminiEmbedder(fake, "gemini-embedding-2-preview", 768)

	_, err := g.Embed(context.Background(), TaskRetrievalDocument, "")
	if err == nil {
		t.Fatal("expected an error for empty text, got nil")
	}
	if fake.called {
		t.Error("expected caller.EmbedContent NOT to be called for empty text")
	}
}

// TestGeminiEmbedder_Embed_WhitespaceOnlyTextErrorsWithoutCallingAPI is the whitespace-only variant of the empty-text fast-fail case.
func TestGeminiEmbedder_Embed_WhitespaceOnlyTextErrorsWithoutCallingAPI(t *testing.T) {
	fake := &fakeEmbedContentCaller{response: vectorResponse(1)}
	g := NewGeminiEmbedder(fake, "gemini-embedding-2-preview", 768)

	_, err := g.Embed(context.Background(), TaskRetrievalDocument, "   \t\n  ")
	if err == nil {
		t.Fatal("expected an error for whitespace-only text, got nil")
	}
	if fake.called {
		t.Error("expected caller.EmbedContent NOT to be called for whitespace-only text")
	}
}

// TestGeminiEmbedder_Embed_WrapsCallerError verifies a caller-side error is surfaced (wrapped), not swallowed.
func TestGeminiEmbedder_Embed_WrapsCallerError(t *testing.T) {
	sentinel := errors.New("upstream boom")
	fake := &fakeEmbedContentCaller{err: sentinel}
	g := NewGeminiEmbedder(fake, "gemini-embedding-2-preview", 768)

	_, err := g.Embed(context.Background(), TaskRetrievalDocument, "some text")
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, sentinel) && !strings.Contains(err.Error(), sentinel.Error()) {
		t.Errorf("expected error to wrap/mention %q, got: %v", sentinel, err)
	}
}

// TestGeminiEmbedder_Embed_ZeroEmbeddingsInResponseErrors verifies a response with no Embeddings produces a clear error rather than an index-out-of-range panic on resp.Embeddings[0].
func TestGeminiEmbedder_Embed_ZeroEmbeddingsInResponseErrors(t *testing.T) {
	fake := &fakeEmbedContentCaller{response: &genai.EmbedContentResponse{Embeddings: nil}}
	g := NewGeminiEmbedder(fake, "gemini-embedding-2-preview", 768)

	_, err := g.Embed(context.Background(), TaskRetrievalDocument, "some text")
	if err == nil {
		t.Fatal("expected an error for a response with zero embeddings, got nil")
	}
}
