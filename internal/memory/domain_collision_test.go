package memory

// This file guards a regression: Classify used to match "code", "word", and "excel" as substrings against the combined app+title text, which collides with ordinary English ("Wordle", "decode", "password", "keyword", "excellent") and misclassified personal browsing as work.
// Fixed by restricting those short/generic terms to app-name-only matching (workAppOnly) — see domain.go.

import "testing"

func TestClassify_ShortGenericWorkTermsDoNotFalsePositiveOnTitleText(t *testing.T) {
	tests := []struct {
		name  string
		app   string
		title string
		want  Domain
	}{
		{"Wordle in a neutral browser is not work", "firefox", "Wordle - New York Times", DomainUnset},
		{"a movie review containing 'excellent' is not work", "firefox", "This is an excellent movie review", DomainUnset},
		{"a password prompt is not work", "firefox", "Enter your password to continue", DomainUnset},
		{"a Morse code tutorial is not work", "firefox", "How to decode Morse signals", DomainUnset},
		{"keyword research tool title is not work", "chrome", "keyword research tool", DomainUnset},
		// The app itself still safely triggers work when it IS one of these short terms — only free-text TITLE matching is restricted.
		{"the actual Word app is still work", "word", "Quarterly Report.docx", DomainWork},
		{"the actual Excel app is still work", "excel", "Budget.xlsx", DomainWork},
		{"the actual Code app is still work", "code", "main.go", DomainWork},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(tt.app, tt.title)
			if got != tt.want {
				t.Errorf("Classify(%q, %q) = %q, want %q", tt.app, tt.title, got, tt.want)
			}
		})
	}
}
