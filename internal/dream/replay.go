// This file is the replay stage: an experimental second read of the day, done one summary at a time by the local shadow model, with plain Go accumulating the answers into per-thread piles. It runs last, after the proven judge-only stages, deliberately — a preempted night still finishes hyp/und/compact and simply skips or partially completes this one. It writes nothing to live memory: the whole deliverable is a markdown artifact under <DataDir>/dreams for the user to read over coffee, plus its calls tracing into the same night's JSONL as every other stage.
package dream

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"ora/internal/brain"
)

const (
	// replayItemCap bounds how many of the night's summary items the replay stage will read, win or lose on the time budget.
	replayItemCap = 300
	// replayBudget bounds how long the replay stage may spend calling the shadow brain before it cuts the night short and writes whatever piles it has.
	replayBudget = 45 * time.Minute
	// replayThreads is how many active thread subjects each per-item call is shown, matching the hypothesis stage's evidence width.
	replayThreads = 30
)

// replayReport is what the replay stage hands the morning report and the artifact header.
type replayReport struct {
	skipped       bool
	partial       bool
	partialReason string // "budget" or "preempted", set only when partial
	items         int
	calls         int
	failures      int // calls that errored or whose reply never parsed — either way the item is skipped, never fatal
	piles         int
	took          time.Duration
	// top holds the highest-scoring piles (subject plus a handful of top facts each), for the diary-writing prompt to draw concrete material from — a taste of the night's second read, not the full artifact.
	top []replayPileSummary
}

// replayPileSummary is one pile's diary-prompt-sized digest: its thread subject and a few of its top facts.
type replayPileSummary struct {
	thread string
	facts  []string
}

// topPiles reduces the night's sorted piles to the handful the diary prompt gets: at most 5 piles, each with at most 5 of its top facts.
func topPiles(sorted []*replayPile) []replayPileSummary {
	const maxPiles, maxFacts = 5, 5
	var out []replayPileSummary
	for _, p := range sorted {
		if len(out) >= maxPiles {
			break
		}
		facts := sortedFacts(p.facts)
		if len(facts) > maxFacts {
			facts = facts[:maxFacts]
		}
		texts := make([]string, len(facts))
		for i, f := range facts {
			texts[i] = f.text
		}
		out = append(out, replayPileSummary{thread: p.thread, facts: texts})
	}
	return out
}

// replayReply is one item's shadow judgement, before the accumulator turns it into pile state.
type replayReply struct {
	Salience int      `json:"salience"`
	Facts    []string `json:"facts"`
	People   []string `json:"people"`
	Thread   string   `json:"thread"`
}

// factEntry is one distinct fact inside a pile: its first-seen wording and how many items said it, case-insensitively.
type factEntry struct {
	text  string
	count int
}

// replayPile is the accumulated state of one thread (or "none") across the night's items: every fact merged case-insensitively with an occurrence count, every person seen with a count, and the pile's score — the sum of the salience of every item that landed in it.
type replayPile struct {
	thread string
	score  int
	facts  map[string]*factEntry // key: strings.ToLower(fact)
	people map[string]int
}

// replayStage reads the night's day of summaries one at a time through the shadow brain and folds the answers into per-thread piles, writing the night's markdown artifact and committing the 'replay' token. It never fails: a store read error aside, every per-item problem (a call error, an unparsable reply, running out of budget, or the user coming back) is absorbed into the report instead of aborting the stage, because a partial night's piles still beat none.
func (r *Runner) replayStage(ctx context.Context, night string) (replayReport, error) {
	var rep replayReport
	if r.activeShadow == nil {
		rep.skipped = true
		slog.Info("dreaming: replay stage skipped, no shadow brain configured", "night", night)
		return rep, nil
	}

	end := r.windowStart(night)
	summaries, err := r.store.SummaryTimeline(ctx, end.Add(-24*time.Hour), end)
	if err != nil {
		return rep, err
	}
	threads, err := r.store.ActiveThreads(ctx, replayThreads)
	if err != nil {
		return rep, err
	}
	subjects := make([]string, len(threads))
	state := make(map[string]string, len(threads))
	for i, t := range threads {
		subjects[i] = t.Subject
		state[t.Subject] = t.State
	}

	started := r.now()
	deadline := started.Add(replayBudget)
	piles := map[string]*replayPile{}
	var order []string

	for _, w := range summaries {
		text, ok := replayItemText(w.Content)
		if !ok {
			continue
		}
		if rep.items >= replayItemCap || r.now().After(deadline) {
			rep.partial, rep.partialReason = true, "budget"
			break
		}
		if ctx.Err() != nil {
			rep.partial, rep.partialReason = true, "preempted"
			break
		}
		rep.items++

		reply, err := r.askShadow(ctx, night, "replay", replayPrompt(text, w.CreatedAt, subjects))
		rep.calls++
		if err != nil {
			rep.failures++
			if ctx.Err() != nil {
				rep.partial, rep.partialReason = true, "preempted"
				break
			}
			continue
		}
		parsed, ok := parseReplayReply(reply)
		if !ok {
			rep.failures++
			continue
		}
		thread := strings.TrimSpace(parsed.Thread)
		if thread == "" || !slices.Contains(subjects, thread) {
			thread = "none"
		}
		accumulateReplay(piles, &order, thread, parsed.Salience, parsed.Facts, parsed.People)
	}
	rep.piles = len(order)
	rep.took = r.now().Sub(started)
	rep.top = topPiles(sortedPiles(piles, order))

	// A preempted or budget-cut night must still land its partial artifact and token: writeCtx drops the cancellation the loop above just observed, so the final write and commit are not themselves cut short by the very preemption they are recording.
	writeCtx := context.WithoutCancel(ctx)
	if err := r.writeReplayArtifact(night, rep, piles, order, state); err != nil {
		slog.Warn("dreaming: writing the replay artifact failed", "night", night, "error", err)
	}
	return rep, r.store.CommitReplayStage(writeCtx, night)
}

// askShadow makes one direct, traced call to the shadow brain under its own generous timeout. Unlike ask/shadowAsk, there is no primary call alongside it: the replay stage's shadow call is the answer, not a comparison echo, so routing it through ask would double up the shadow's work for nothing.
func (r *Runner) askShadow(ctx context.Context, night, kind, prompt string) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, shadowTimeout)
	defer cancel()
	reply, err := r.activeShadow(callCtx, prompt)
	r.traceCall(night, kind, reply, err)
	return reply, err
}

// parseReplayReply decodes one item's shadow reply with the same fence-strip and outermost-JSON recovery askJSON uses, since a local model pads its answer with prose exactly as often as a hosted one does.
func parseReplayReply(reply string) (replayReply, bool) {
	var out replayReply
	body := brain.StripFence(reply)
	if json.Unmarshal([]byte(body), &out) == nil {
		return out, true
	}
	if sliced := brain.OutermostJSON(body); sliced != "" && json.Unmarshal([]byte(sliced), &out) == nil {
		return out, true
	}
	return replayReply{}, false
}

// replayItemText pulls the prose out of one summary node for the replay call, false for a "Raw Activity Log" bucket — noise the stage skips outright, same as the hypothesis stage's evidence material.
func replayItemText(content string) (string, bool) {
	var t struct {
		Task    string `json:"task_name"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal([]byte(content), &t); err == nil && t.Task != "" {
		if t.Task == "Raw Activity Log" {
			return "", false
		}
		if t.Summary != "" {
			return t.Task + " — " + t.Summary, true
		}
	}
	return content, true
}

// accumulateReplay folds one item's judgement into its pile: salience 0 drops the item entirely (it never even creates its pile), a new thread appends to order so piles render in first-seen order before the score sort, facts merge case-insensitively with a running count, and people accumulate the same way.
func accumulateReplay(piles map[string]*replayPile, order *[]string, thread string, salience int, facts, people []string) {
	if salience <= 0 {
		return
	}
	p, ok := piles[thread]
	if !ok {
		p = &replayPile{thread: thread, facts: map[string]*factEntry{}, people: map[string]int{}}
		piles[thread] = p
		*order = append(*order, thread)
	}
	p.score += salience
	for _, f := range facts {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		key := strings.ToLower(f)
		if e, ok := p.facts[key]; ok {
			e.count++
		} else {
			p.facts[key] = &factEntry{text: f, count: 1}
		}
	}
	for _, person := range people {
		person = strings.TrimSpace(person)
		if person == "" {
			continue
		}
		p.people[person]++
	}
}

// replayInstruction heads every per-item replay call. Each call judges one moment alone, with no memory of the items around it — that isolation is what makes this a second, independent read of the day rather than a rerun of the same running-summary judgement the compiler already made.
const replayInstruction = `You are Ora, an ambient companion that watches the user's day through their screen. You are replaying one moment from today in isolation — judge only what is in front of you, not what a moment nearby might suggest.

Principles:
- salience is how much this moment matters, 0 to 3: 0 is nothing worth keeping, 3 is a fact that will still matter weeks from now.
- facts is a short list of standalone facts this moment establishes; an empty list when there are none.
- people is every person's name this moment mentions; an empty list when there are none.
- thread is the one active thread below this moment belongs to, its subject copied verbatim, or the literal string "none" when it fits none of them.
- Answer with only JSON, no prose around it: {"salience": 0-3, "facts": ["..."], "people": ["..."], "thread": "<subject or none>"}.`

// replayPrompt assembles one per-item call: the instruction, the moment's timestamp and text, and the night's active thread subjects, numbered so the reply can name one back verbatim.
func replayPrompt(text string, at time.Time, subjects []string) string {
	var b strings.Builder
	b.WriteString(replayInstruction)
	fmt.Fprintf(&b, "\n\n--- The moment, %s ---\n%s\n", at.Local().Format("Jan 2 15:04"), text)
	b.WriteString("\n--- Active threads ---\n")
	if len(subjects) == 0 {
		b.WriteString("(none)\n")
	}
	for i, s := range subjects {
		fmt.Fprintf(&b, "%d. %s\n", i+1, s)
	}
	return b.String()
}

// writeReplayArtifact writes <DataDir>/dreams/<night>-replay.md, the whole deliverable of the stage: a header of what the night managed, then every pile sorted by score with its facts and people, the "none" pile last regardless of its score since it is a catch-all rather than a thread. A DataDir of "" disables the write, same as traceCall — there is nowhere to put it.
func (r *Runner) writeReplayArtifact(night string, rep replayReport, piles map[string]*replayPile, order []string, state map[string]string) error {
	if r.DataDir == "" {
		return nil
	}
	sorted := sortedPiles(piles, order)

	var b strings.Builder
	fmt.Fprintf(&b, "# Replay — night of %s\n\n", night)
	fmt.Fprintf(&b, "Items read: %d\n", rep.items)
	fmt.Fprintf(&b, "Calls made: %d\n", rep.calls)
	fmt.Fprintf(&b, "Parse failures: %d\n", rep.failures)
	fmt.Fprintf(&b, "Duration: %s\n", rep.took.Round(time.Second))
	if rep.partial {
		fmt.Fprintf(&b, "Budget: hit (%s)\n", rep.partialReason)
	} else {
		b.WriteString("Budget: not hit\n")
	}

	for _, p := range sorted {
		fmt.Fprintf(&b, "\n## %s (score %d)\n", p.thread, p.score)
		if s := state[p.thread]; s != "" {
			fmt.Fprintf(&b, "State: %s\n", s)
		}
		b.WriteString("\n")
		if len(p.facts) == 0 {
			b.WriteString("(no facts)\n")
		}
		for _, f := range sortedFacts(p.facts) {
			fmt.Fprintf(&b, "- %s (x%d)\n", f.text, f.count)
		}
		if len(p.people) > 0 {
			b.WriteString("\nPeople: ")
			b.WriteString(strings.Join(sortedPeople(p.people), ", "))
			b.WriteString("\n")
		}
	}

	dir := filepath.Join(r.DataDir, "dreams")
	// 0600 in a 0700 directory, matching the night trace next to it: the artifact holds the same raw brain replies and people's names.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("replay artifact dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, night+"-replay.md"), []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("write replay artifact: %w", err)
	}
	return nil
}

// sortedPiles orders piles by score descending, ties broken by first-seen order, with the "none" catch-all always last regardless of its score.
func sortedPiles(piles map[string]*replayPile, order []string) []*replayPile {
	out := make([]*replayPile, 0, len(order))
	for _, t := range order {
		out = append(out, piles[t])
	}
	slices.SortStableFunc(out, func(a, b *replayPile) int {
		if a.thread == "none" {
			return 1
		}
		if b.thread == "none" {
			return -1
		}
		return b.score - a.score
	})
	return out
}

// sortedFacts orders one pile's facts by occurrence count descending, ties broken alphabetically on the fact's first-seen wording, so the artifact renders deterministically.
func sortedFacts(facts map[string]*factEntry) []*factEntry {
	out := make([]*factEntry, 0, len(facts))
	for _, f := range facts {
		out = append(out, f)
	}
	slices.SortStableFunc(out, func(a, b *factEntry) int {
		if a.count != b.count {
			return b.count - a.count
		}
		return strings.Compare(a.text, b.text)
	})
	return out
}

// sortedPeople renders one pile's people as "name" or "name (xN)" for N>1, most-mentioned first, ties broken alphabetically.
func sortedPeople(people map[string]int) []string {
	type kv struct {
		name  string
		count int
	}
	kvs := make([]kv, 0, len(people))
	for name, count := range people {
		kvs = append(kvs, kv{name, count})
	}
	slices.SortStableFunc(kvs, func(a, b kv) int {
		if a.count != b.count {
			return b.count - a.count
		}
		return strings.Compare(a.name, b.name)
	})
	out := make([]string, len(kvs))
	for i, k := range kvs {
		if k.count > 1 {
			out[i] = k.name + " (x" + strconv.Itoa(k.count) + ")"
		} else {
			out[i] = k.name
		}
	}
	return out
}

// replayLine renders the morning report's one line about the replay stage: skipped, partial with its reason, or the count of items folded into piles.
func replayLine(rep replayReport) string {
	switch {
	case rep.skipped:
		return "Replay skipped: no shadow brain configured.\n"
	case rep.partial:
		return fmt.Sprintf("Replay was partial (%s): replayed %d items into %d piles.\n", rep.partialReason, rep.items, rep.piles)
	default:
		return fmt.Sprintf("I replayed %d items into %d piles.\n", rep.items, rep.piles)
	}
}
