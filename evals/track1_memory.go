package main

// Track 1 replays real user questions through the retrieval the live agent actually runs. query_memory (internal/agent/tools.go, the "query_memory" case) calls brain.HybridSearchWindow(ctx, query, domain, since, until, limit) and shows the model db.FormatHit of the top ten; directSearch calls that same retrieval directly, and toolPathSearch (-tool-path) goes one layer higher through agent.ExecuteTool so the tool's own arg handling and answers are what gets judged. The store is a snapshot copy of the live sqlite file; the vector half is the daemon's own chromem index reached over /vector/search, because two processes must never open that directory at once.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"ora/internal/agent"
	"ora/internal/db"
)

// queryMemoryHits mirrors internal/agent/tools.go's constant of the same name: how many hits query_memory puts in front of the model.
const queryMemoryHits = 10

// question is one row of questions.jsonl: a real thing the user asked, and where it came from. Args, when set, are extra query_memory arguments (since, until, domain, app) the tool-path replay passes alongside the question text — this is how a golden question pins the time window the model would have chosen.
type question struct {
	ID       string         `json:"id"`
	Origin   string         `json:"origin"`
	Question string         `json:"question"`
	Args     map[string]any `json:"args,omitempty"`
}

// track1Result is one question's replay: the hits retrieval returned, and the judge's call on whether a companion could answer from them.
type track1Result struct {
	question
	Hits    []string
	Latency time.Duration
	Err     string
	V       verdict
}

const track1Instruction = `You are judging a personal AI companion's memory retrieval, not its writing.

You are given a question the user really asked, and the memory rows the companion's retrieval returned for it. The rows are its whole view of the past for that turn: everything it could truthfully say comes from these rows and nothing else.

Decide one thing: could a companion answer this question from these rows, in a way the user would accept?

Judge intention, not string overlap. A row that names the same work in different words counts. A row that is on the right subject but from the wrong time does not answer a question about a specific window. Rows that are merely near the topic, with none of them carrying the specific thing asked for, do not count as an answer. If the question asks how the companion knows something, the rows must show the provenance, not just the fact. If the question is a follow-up whose subject lives in the previous turn, judge whether the rows would let the companion continue that thread at all.

An empty result set is a fail unless the honest answer really is "nothing happened", in which case say so.

Reply with JSON only: {"verdict":"pass"|"fail","why":"one clause, under 20 words"}`

// loadQuestions reads questions.jsonl. Input: path to the file. Output: the questions in file order.
func loadQuestions(path string) ([]question, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var qs []question
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		var q question
		if err := json.Unmarshal([]byte(line), &q); err != nil {
			return nil, fmt.Errorf("questions.jsonl: %w", err)
		}
		qs = append(qs, q)
	}
	return qs, sc.Err()
}

// track1Search resolves one question into the formatted rows the judge scores. Input: the question. Output: the rows the companion's retrieval put in front of the model, empty when it honestly found nothing.
type track1Search func(ctx context.Context, q question) ([]string, error)

// directSearch replays a question straight through store.HybridSearch and db.FormatHit — the retrieval call query_memory makes, without the tool dispatch around it.
func directSearch(store *db.Store) track1Search {
	return func(ctx context.Context, q question) ([]string, error) {
		hits, err := store.HybridSearch(ctx, q.Question, "", queryMemoryHits)
		if err != nil {
			return nil, err
		}
		rows := make([]string, 0, len(hits))
		for _, h := range hits {
			rows = append(rows, db.FormatHit(h, 0))
		}
		return rows, nil
	}
}

// toolPathSearch replays a question through the agent's real query_memory tool (agent.ExecuteTool — the exact dispatch the model's function calls take), so the judge scores what the model would actually see: the tool's formatting, its arg handling, and its honest none-in-window answers. The question's Args are passed alongside its text, letting a golden question carry the since/until window the model would have chosen.
func toolPathSearch(a *agent.Agent) track1Search {
	return func(ctx context.Context, q question) ([]string, error) {
		args := map[string]any{"query": q.Question}
		for k, v := range q.Args {
			args[k] = v
		}
		res := a.ExecuteTool(ctx, "query_memory", args)
		if strings.HasPrefix(res, "error: ") {
			return nil, fmt.Errorf("query_memory: %s", strings.TrimPrefix(res, "error: "))
		}
		if res == "no memory matches" {
			return nil, nil
		}
		return strings.Split(res, "\n"), nil
	}
}

// runTrack1 replays every question through search and has the judge score each result set. Input: the search path to measure (directSearch or toolPathSearch), the question set. Output: one result per question.
func runTrack1(ctx context.Context, search track1Search, j *judge, qs []question) []track1Result {
	results := make([]track1Result, 0, len(qs))
	for _, q := range qs {
		r := track1Result{question: q}
		start := time.Now()
		rows, err := search(ctx, q)
		r.Latency = time.Since(start)
		if err != nil {
			r.Err = err.Error()
			r.V = verdict{Verdict: "fail", Why: "retrieval errored"}
			results = append(results, r)
			fmt.Printf("  [%-3s] ERROR %v\n", q.ID, err)
			continue
		}
		r.Hits = rows

		material := fmt.Sprintf("QUESTION: %s\n\nRETRIEVED ROWS (%d):\n%s",
			q.Question, len(r.Hits), rowsOrNone(r.Hits))
		if err := j.ask(ctx, track1Instruction, material, &r.V); err != nil {
			r.Err = err.Error()
			r.V = verdict{Verdict: "fail", Why: "judge call failed"}
		}
		fmt.Printf("  [%-3s] %-4s %2d hits %6dms  %s\n", q.ID, r.V.Verdict, len(r.Hits), r.Latency.Milliseconds(), r.V.Why)
		results = append(results, r)
	}
	return results
}

// rowsOrNone renders retrieved rows for the judge, saying plainly when there were none rather than handing the judge an empty block it has to interpret.
func rowsOrNone(rows []string) string {
	if len(rows) == 0 {
		return "(none — retrieval returned nothing)"
	}
	var b strings.Builder
	for i, r := range rows {
		fmt.Fprintf(&b, "%d. %s\n", i+1, r)
	}
	return b.String()
}
