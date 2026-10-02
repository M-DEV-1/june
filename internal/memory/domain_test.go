package memory

// One row per branch of Classify: app lists, title match in a neutral browser, case folding, a signal on both lists, and the short work terms that only count as an app name.

import "testing"

func TestClassify(t *testing.T) {
	tests := []struct {
		name  string
		app   string
		title string
		want  Domain
	}{
		// --- clearly work ---
		{"Slack app is work", "Slack", "general", DomainWork},

		// --- clearly personal ---
		{"Netflix app is personal", "Netflix", "The Bear S3E1", DomainPersonal},

		// --- ambiguous / unknown -> unset ---
		{"a made-up app name is unset", "QuantumFlibberWidget", "some window", DomainUnset},

		// --- matches via title, not app ---
		{"work title in a neutral browser classifies work via title", "firefox", "Slack | general", DomainWork},

		// --- case-insensitivity ---
		{"uppercase SLACK classifies work", "SLACK", "general", DomainWork},

		// --- matches both lists -> unset, don't guess ---
		{
			"a signal matching both work and personal lists resolves unset",
			"firefox",
			"Slack chat about Netflix later",
			DomainUnset,
		},

		// --- short generic work terms in title do not false-positive (workAppOnly) ---
		{"Wordle in a neutral browser is not work", "firefox", "Wordle - New York Times", DomainUnset},
		{"the actual Word app is still work", "word", "Quarterly Report.docx", DomainWork},
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
