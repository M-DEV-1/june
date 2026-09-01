// This file recovers the JSON out of a model's reply. Every backend here answers in prose by habit — a sentence before the answer, a markdown fence around it, a thought after it — and each caller that asks for JSON was writing the same two recoveries by hand.
package brain

import (
	"encoding/json"
	"strings"
)

// StripFence removes a markdown code fence from around a reply body, with or without a language tag, and returns the body inside. A reply with no fence comes back unchanged. Wrapping an answer in ``` is the most common way a JSON reply is lost.
func StripFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```")
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:] // drop the language tag, if any
	}
	if i := strings.LastIndex(s, "```"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// OutermostJSON slices s down to the first JSON value it contains that actually parses, or "" when it holds none — the recovery for a reply that says a sentence and then answers.
// Each "[" or "{" in turn is tried as a start and decoded; the first that yields a complete value wins. Matching brackets by position instead would take the earliest one and the last closing bracket anywhere after it, so a reply reading "Based on the notes [see above], here is: {\"items\": []}" would return everything from "[see above]" to a bracket inside the object — a slice that encloses the answer without being valid JSON.
func OutermostJSON(s string) string {
	for i := strings.IndexAny(s, "[{"); i >= 0; {
		var v json.RawMessage
		if err := json.NewDecoder(strings.NewReader(s[i:])).Decode(&v); err == nil {
			return strings.TrimSpace(string(v))
		}
		next := strings.IndexAny(s[i+1:], "[{")
		if next < 0 {
			break
		}
		i += 1 + next
	}
	return ""
}
