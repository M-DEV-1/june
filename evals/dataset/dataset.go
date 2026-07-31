// dataset holds the eval fixtures.
// real facts pulled from this repo's own past conversations.
// each fact has a few probe queries a user might ask later that should find it again.
package dataset

import (
	_ "embed"
	"encoding/json"
)

//go:embed facts.json
var factsJSON []byte

// Probe = one query + the substrings we expect to see back (case-insensitive).
type Probe struct {
	Query          string   `json:"query"`
	ExpectContains []string `json:"expect_contains"`
}

// Fact = one real thing ORA should remember.
type Fact struct {
	ID            string  `json:"id"`
	SourceSession string  `json:"source_session"`
	Kind          string  `json:"kind"` // "note" or "thread"
	Content       string  `json:"content"`
	Probes        []Probe `json:"probes"`
}

// Load reads facts.json (embedded at build time) into []Fact.
func Load() ([]Fact, error) {
	var facts []Fact
	if err := json.Unmarshal(factsJSON, &facts); err != nil {
		return nil, err
	}
	return facts, nil
}
