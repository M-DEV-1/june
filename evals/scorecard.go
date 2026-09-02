package main

// The scorecard is the run's output: one markdown file per run in evals/runs/, named for the date and the commit it measured. Two files from two commits diffed against each other are the regression signal — same questions, same criteria, different code.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// writeScorecard renders the run as markdown and writes it to outDir. Input: the run's results and the output directory. Output: the path written.
func writeScorecard(c scorecard, outDir string) (string, error) {
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return "", err
	}
	// To the minute for the same reason the frozen rows are: measuring a fix means running the same commit twice in an evening, and a day-and-sha name overwrites the baseline being measured against.
	path := filepath.Join(outDir, fmt.Sprintf("%s-%s.md", c.Started.Format("2006-01-02-1504"), c.SHA))

	var b strings.Builder
	fmt.Fprintf(&b, "# Ora eval scorecard — %s\n\n", c.Started.Format("2006-01-02 15:04 MST"))
	fmt.Fprintf(&b, "Commit `%s`. Tracks run: %s.\n\n", c.SHA, ranTracks(c.Ran))
	for _, n := range c.Notes {
		fmt.Fprintf(&b, "> Note: %s\n\n", n)
	}
	b.WriteString(summary(c))
	b.WriteString("\n")

	if c.Ran["1"] {
		writeTrack1Section(&b, c.T1)
	}
	if c.Ran["2"] {
		writeTrack2Section(&b, c.T2)
	}
	if c.Ran["3"] {
		writeTrack3Section(&b, c.T3)
	}
	if c.Ran["8"] {
		writeTrack8Section(&b, c.T8, c.T8Arms)
	}
	return path, os.WriteFile(path, []byte(b.String()), 0644)
}

// summary is the headline block, printed to stdout and repeated at the top of the file: the three numbers a reader should see before anything else.
func summary(c scorecard) string {
	var b strings.Builder
	b.WriteString("## Headline\n\n")
	b.WriteString("| Track | Measures | Score |\n|---|---|---|\n")
	if c.Ran["1"] {
		p, n := track1Rate(c.T1)
		b.WriteString(fmt.Sprintf("| 1 memory replay | questions whose retrieved rows could answer them | %s |\n", pct(p, n)))
	}
	if c.Ran["2"] {
		p, n := track2Rate(c.T2)
		b.WriteString(fmt.Sprintf("| 2 conversation | taste criteria passed across scored turns | %s |\n", pct(p, n)))
	}
	if c.Ran["3"] {
		p, n := track3Rate(c.T3)
		b.WriteString(fmt.Sprintf("| 3 minutes | attendee and taste criteria passed across minutes files | %s |\n", pct(p, n)))
	}
	if c.Ran["8"] {
		p, n := track8SufficientRate(c.T8)
		b.WriteString(fmt.Sprintf("| 8 context vs capacity | questions whose retrieved rows were sufficient | %s |\n", pct(p, n)))
		for _, a := range c.T8Arms {
			p, n := track8ArmRate(c.T8, a.Name)
			b.WriteString(fmt.Sprintf("| 8 — %s | answered well from those same rows | %s |\n", a.Name, pct(p, n)))
		}
	}
	return b.String()
}

func ranTracks(sel map[string]bool) string {
	var out []string
	for _, t := range []string{"1", "2", "3", "5", "6", "7"} {
		if sel[t] {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return "none"
	}
	return strings.Join(out, ", ")
}

// track1Rate is how many questions the judge said were answerable from what retrieval returned.
func track1Rate(rs []track1Result) (passes, total int) {
	for _, r := range rs {
		total++
		if r.V.passed() {
			passes++
		}
	}
	return passes, total
}

// track2Rate is every taste verdict across every turn, pooled — the one number that moves when the conversation layer gets better.
func track2Rate(rs []track2Result) (passes, applicable int) {
	for _, r := range rs {
		p, a := rate(collect(r.Taste, tasteIDs))
		passes += p
		applicable += a
	}
	return passes, applicable
}

func track3Rate(rs []track3Result) (passes, applicable int) {
	for _, r := range rs {
		p, a := rate(collect(r.Criteria, minutesIDs))
		passes += p
		applicable += a
	}
	return passes, applicable
}

func writeTrack1Section(b *strings.Builder, rs []track1Result) {
	b.WriteString("\n## Track 1 — memory replay\n\n")
	b.WriteString("Each question is a thing the user really asked (D-cases from the seed corpus, L-cases harvested from ora.log). Retrieval is `db.Store.HybridSearch(question, \"\", 10)` — the retrieval `query_memory` runs (with `-tool-path`, the replay goes through the agent's real `query_memory` tool via ExecuteTool, honoring each question's args, e.g. a since/until window — see the run notes) — against a VACUUM INTO snapshot of the live store, with the daemon's own embedder and vector index over IPC. The judge sees the question and the formatted rows and says whether a companion could answer from them.\n\n")
	b.WriteString("| Q | Question | Hits | Latency | Verdict | Judge |\n|---|---|---|---|---|---|\n")
	for _, r := range rs {
		fmt.Fprintf(b, "| %s | %s | %d | %dms | %s | %s |\n",
			r.ID, cell(r.Question), len(r.Hits), r.Latency.Milliseconds(), r.V.Verdict, cell(r.V.Why))
	}
	b.WriteString("\n### Failures in full\n\n")
	any := false
	for _, r := range rs {
		if r.V.passed() {
			continue
		}
		any = true
		fmt.Fprintf(b, "**%s — %q** (%s)\n\n%s\n\nRows returned (%d):\n\n", r.ID, r.Question, r.Origin, r.V.Why, len(r.Hits))
		if len(r.Hits) == 0 {
			b.WriteString("- (nothing)\n")
		}
		for _, h := range r.Hits {
			fmt.Fprintf(b, "- %s\n", cell(truncateRunes(h, 220)))
		}
		if r.Err != "" {
			fmt.Fprintf(b, "\nError: %s\n", r.Err)
		}
		b.WriteString("\n")
	}
	if !any {
		b.WriteString("None.\n")
	}
}

func writeTrack2Section(b *strings.Builder, rs []track2Result) {
	b.WriteString("\n## Track 2 — conversation judge\n\n")
	fmt.Fprintf(b, "Turn pairs are read out of `ora.log`: every `\"ora said\"` line (written by internal/agent/connect.go on a completed or interrupted spoken turn) paired with the last user turn within five minutes before it. That logging began 2026-08-28, so this corpus starts there and grows. %d turns scored.\n\n", len(rs))

	b.WriteString("| Criterion | Pass rate |\n|---|---|\n")
	for _, id := range tasteIDs {
		var vs []verdict
		for _, r := range rs {
			if v, ok := r.Taste[id]; ok {
				vs = append(vs, v)
			}
		}
		p, a := rate(vs)
		fmt.Fprintf(b, "| %s — %s | %s |\n", id, tasteNames[id], pct(p, a))
	}

	b.WriteString("\n### Worst turns\n\n")
	worst := worstTurns(rs, 5)
	if len(worst) == 0 {
		b.WriteString("No turn failed a criterion.\n")
	}
	for _, r := range worst {
		fmt.Fprintf(b, "**%s** — failed %s\n\n", r.At.Format("2006-01-02 15:04:05"), joinIDs(r.failedTaste()))
		if r.UserText != "" {
			fmt.Fprintf(b, "> user (%s): %s\n>\n", r.UserMode, cell(r.UserText))
		} else {
			b.WriteString("> (session opening — Ora spoke first)\n>\n")
		}
		fmt.Fprintf(b, "> ora: %s\n\n", cell(r.OraText))
		for _, id := range r.failedTaste() {
			if v, ok := r.Taste[id]; ok {
				fmt.Fprintf(b, "- %s: %s\n", id, cell(v.Why))
				continue
			}
			for _, d := range r.DCases {
				if d.Case == id {
					fmt.Fprintf(b, "- %s: %s\n", id, cell(d.Why))
				}
			}
		}
		b.WriteString("\n")
	}

	b.WriteString("### D-case criteria the judge found applicable\n\n")
	counts := map[string][2]int{}
	for _, r := range rs {
		for _, d := range r.DCases {
			c := counts[d.Case]
			c[1]++
			if strings.EqualFold(d.Verdict, "pass") {
				c[0]++
			}
			counts[d.Case] = c
		}
	}
	if len(counts) == 0 {
		b.WriteString("None matched.\n")
	} else {
		b.WriteString("| Case | Pass rate |\n|---|---|\n")
		for _, id := range []string{"D1", "D2", "D6", "D7", "D8", "D9", "D10", "D13", "D14", "D15"} {
			if c, ok := counts[id]; ok {
				fmt.Fprintf(b, "| %s | %s |\n", id, pct(c[0], c[1]))
			}
		}
	}
}

// tasteNames labels each taste criterion so the scorecard reads without the rubric alongside it.
var tasteNames = map[string]string{
	"T1":  "no machine identifier spoken",
	"T2":  "projects named as the user names them",
	"T3":  "every number is one the user chose",
	"T4":  "no hedge on a supported claim",
	"T5":  "no tool, memory or API mentioned",
	"T6":  "answers the question asked",
	"T7":  "every fact traces to this turn's evidence",
	"T8":  "relates activities or names one",
	"T9":  "idle time said as a person would",
	"T10": "second person, no assistant vocabulary",
}

// worstTurns returns up to n turns ordered by how many criteria they failed.
func worstTurns(rs []track2Result, n int) []track2Result {
	sorted := append([]track2Result(nil), rs...)
	for i := 1; i < len(sorted); i++ {
		for k := i; k > 0 && len(sorted[k].failedTaste()) > len(sorted[k-1].failedTaste()); k-- {
			sorted[k], sorted[k-1] = sorted[k-1], sorted[k]
		}
	}
	var out []track2Result
	for _, r := range sorted {
		if len(r.failedTaste()) == 0 || len(out) >= n {
			break
		}
		out = append(out, r)
	}
	return out
}

func writeTrack3Section(b *strings.Builder, rs []track3Result) {
	b.WriteString("\n## Track 3 — minutes judge\n\n")
	b.WriteString("Every `minutes.md` under the recordings directory, scored with its `transcript.md` alongside, against the attendee and presence rules in internal/recorder/minutes.go.\n\n")
	b.WriteString("| Criterion | Pass rate |\n|---|---|\n")
	for _, id := range minutesIDs {
		var vs []verdict
		for _, r := range rs {
			if v, ok := r.Criteria[id]; ok {
				vs = append(vs, v)
			}
		}
		p, a := rate(vs)
		fmt.Fprintf(b, "| %s — %s | %s |\n", id, minutesNames[id], pct(p, a))
	}
	b.WriteString("\n| Meeting | Score | Failed |\n|---|---|---|\n")
	for _, r := range rs {
		p, a := rate(collect(r.Criteria, minutesIDs))
		fmt.Fprintf(b, "| %s | %s | %s |\n", filepath.Base(r.Dir), pct(p, a), joinIDs(r.failedMinutes()))
	}
	b.WriteString("\n### Why each failed\n\n")
	for _, r := range rs {
		if len(r.failedMinutes()) == 0 && r.Err == "" {
			continue
		}
		fmt.Fprintf(b, "**%s**\n\n", filepath.Base(r.Dir))
		if r.Err != "" {
			fmt.Fprintf(b, "- error: %s\n", r.Err)
		}
		for _, id := range r.failedMinutes() {
			fmt.Fprintf(b, "- %s (%s): %s\n", id, minutesNames[id], cell(r.Criteria[id].Why))
		}
		b.WriteString("\n")
	}
}

// cell makes a string safe for a markdown table cell: newlines flattened and pipes escaped, so one multi-line transcript quote cannot break the table it sits in.
func cell(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "|", "\\|")
	return strings.TrimSpace(s)
}
