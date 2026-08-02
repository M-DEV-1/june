package vector

import "testing"

// TestResult_FieldAccess just proves Result's shape is sound and the package compiles.
func TestResult_FieldAccess(t *testing.T) {
	r := Result{
		ID:         "doc-1",
		Content:    "the user was debugging a CUDA out-of-memory error",
		Metadata:   map[string]string{"domain": "work"},
		Similarity: 0.87,
	}

	if r.ID != "doc-1" {
		t.Errorf("ID = %q, want %q", r.ID, "doc-1")
	}
	if r.Content == "" {
		t.Error("expected non-empty Content")
	}
	if r.Metadata["domain"] != "work" {
		t.Errorf("Metadata[domain] = %q, want %q", r.Metadata["domain"], "work")
	}
	if r.Similarity != 0.87 {
		t.Errorf("Similarity = %v, want %v", r.Similarity, 0.87)
	}
}
