package memory

// Table-driven coverage for Classify's work/personal/unset lookup.

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
		{"VS Code (Code) app is work", "Code", "main.go", DomainWork},
		{"Zoom with a meeting title is work", "Zoom", "Weekly Sync Meeting", DomainWork},
		{"a terminal app is work", "iTerm2", "~/projects/ora", DomainWork},
		{"Outlook app is work", "Outlook", "Inbox", DomainWork},

		// --- clearly personal ---
		{"Netflix app is personal", "Netflix", "The Bear S3E1", DomainPersonal},
		{"Steam app is personal", "Steam", "Library", DomainPersonal},
		{"Spotify app is personal", "Spotify", "Discover Weekly", DomainPersonal},
		{"Instagram app is personal", "Instagram", "Home", DomainPersonal},

		// --- ambiguous / unknown -> unset ---
		{"bare firefox with generic title is unset", "firefox", "New Tab", DomainUnset},
		{"bare chrome with generic title is unset", "chrome", "Google Search", DomainUnset},
		{"empty app and title is unset", "", "", DomainUnset},
		{"a made-up app name is unset", "QuantumFlibberWidget", "some window", DomainUnset},

		// --- matches via title, not app ---
		{"work title in a neutral browser classifies work via title", "firefox", "Slack | general", DomainWork},

		// --- case-insensitivity ---
		{"uppercase SLACK classifies work", "SLACK", "general", DomainWork},
		{"lowercase slack classifies work", "slack", "general", DomainWork},
		{"mixed-case Slack classifies work", "Slack", "general", DomainWork},

		// --- matches both lists -> unset, don't guess ---
		{
			"a signal matching both work and personal lists resolves unset",
			"firefox",
			"Slack chat about Netflix later",
			DomainUnset,
		},
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
