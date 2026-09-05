package brain

import "testing"

// TestGrokNote_IsNonEmpty checks the sentence /brains hands the window for the Grok row's limits_note actually says something, rather than the zero string a forgotten case would leave behind.
func TestGrokNote_IsNonEmpty(t *testing.T) {
	if GrokNote() == "" {
		t.Fatalf("GrokNote() is empty, want a sentence explaining Grok exposes no usage data")
	}
}
