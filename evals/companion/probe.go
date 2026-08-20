package companion

import (
	"context"
	"strings"

	"ora/evals/dataset"
	"ora/internal/agent"
	"ora/internal/db"
)

// Probe is the offline diagnosis for one question: could the frozen handshake answer it, would a typed RetrieveRelevant inject it, and would query_memory/get_recent find it if the model actually called a tool.
type Probe struct {
	Question     dataset.Question
	HandshakeHas bool
	RetrieveHas  bool
	QueryHas     bool
	RecentHas    bool
	Handshake    []string
	Retrieve     []string
	Query        string
	Recent       string
	Diagnosis    string
}

// ProbeCache runs the production retrieval surfaces against store for every question, without calling Gemini. Questions are stamped against the store clock first so recency is relative to the latest memory row, not wall-clock.
func ProbeCache(ctx context.Context, store *db.Store, questions []dataset.Question) []Probe {
	questions, _ = dataset.Stamp(ctx, store, questions)
	a := agent.NewAgent(nil, nil, store, nil, "")
	out := make([]Probe, 0, len(questions))
	handshake, _ := store.GetImplicitContext(ctx)
	for _, q := range questions {
		p := Probe{
			Question:  q,
			Handshake: handshake,
		}
		p.HandshakeHas = containsAllFold(strings.Join(handshake, "\n"), q.Expect)

		recalls, err := store.RetrieveRelevant(ctx, q.Ask, 2)
		if err == nil {
			p.Retrieve = recalls
			p.RetrieveHas = containsAllFold(strings.Join(recalls, "\n"), q.Expect)
		}

		p.Query = a.ExecuteTool(ctx, "query_memory", map[string]any{"query": q.Ask})
		p.QueryHas = containsAllFold(p.Query, q.Expect)

		p.Recent = a.ExecuteTool(ctx, "get_recent", map[string]any{"limit": 10})
		p.RecentHas = containsAllFold(p.Recent, q.Expect)

		p.Diagnosis = diagnose(p)
		out = append(out, p)
	}
	return out
}

func diagnose(p Probe) string {
	when := p.Question.Horizon
	if p.Question.AgeLabel != "" {
		when += " · " + p.Question.AgeLabel
	}
	where := "not in store on these surfaces — retrieval itself misses"
	switch {
	case p.HandshakeHas:
		where = "handshake has it — frozen cache would answer if the model reads it"
	case p.RetrieveHas:
		where = "handshake miss, typed RetrieveRelevant would inject it — voice never does this"
	case p.QueryHas:
		where = "only findable via query_memory — model must call a tool"
	case p.RecentHas:
		where = "only findable via get_recent — model must call a tool"
	}
	if when != "" {
		return when + " — " + where
	}
	return where
}
