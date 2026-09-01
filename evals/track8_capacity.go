package main

// Track 8 answers the one question a single score cannot: when a question goes unanswered, was the material missing or was the model unable to use it?
// It measures the two independently. Retrieval runs once and its rows are frozen to disk; a judge says whether those rows were sufficient, and separately every arm answers the question from those same frozen rows and is judged on what it said. Crossing the two gives four cells, and each licenses a different conclusion — the design is Google Research's "sufficient context" cross-tab (arXiv:2411.06037), whose autorater agreed with human labels 93% of the time.
// The arms are whole model families rather than one vendor's ladder. A gap between a small and a large model from one lab is confounded by that lab being weak at this shape of question; three families failing the same rows is not.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ora/internal/brain"
	"ora/internal/config"
)

// answerInstruction is what each arm is given. It is deliberately strict about the rows being the arm's whole world: an arm that answers from what it happens to know rather than from the rows would score as a retrieval success and mask the failure, which is the cell this track exists to expose.
const answerInstruction = `You are a personal assistant answering the user's question from their own recorded history.

The MEMORY ROWS below are everything you know. They are the complete record available to you for this question. You have no other knowledge of this user, their work, their colleagues, or their past.

Answer the question using only those rows.

If the rows do not contain what the question asks for, say exactly: INSUFFICIENT
Do not guess. Do not fill a gap from general knowledge. Do not answer a question the rows cannot support just because it seems answerable.
If the question itself assumes something the rows contradict, say so plainly instead of answering it.

Otherwise answer in one to three sentences, plainly, as you would say it out loud.`

// answerJudgeInstruction scores one arm's answer. Correctness and groundedness are one verdict on purpose: an answer that is true but not supported by the rows is the masked-retrieval-failure case, and counting it as a pass is exactly the mistake that makes a single-metric scorecard read better than the system is.
const answerJudgeInstruction = `You are scoring one answer a personal AI companion gave, against the memory rows it was given and nothing else.

You see the user's question, the rows the companion was shown, and the answer it produced.

Score one thing: is this a good answer to that question, supported by those rows?

  "pass" — the answer addresses the question and everything it asserts is supported by the rows. Declining is a pass when the rows genuinely cannot support an answer, and so is correcting a question whose premise the rows contradict.
  "fail" — the answer is wrong, or it asserts something the rows do not support. An answer that happens to be true but is not in the rows is a fail: the companion guessed, and the guess would break on any fact it had not been trained on. Declining is also a fail when the rows did hold the answer.

Judge only these two. Whether the companion refused is not your concern and is read from its own words elsewhere; you are scoring whether what it said was a good response to that question given those rows.

Judge intention, not wording. An answer that names the same thing in different words is supported. Do not reward length.

Reply with JSON only: {"verdict":"pass"|"fail","why":"one clause, under 20 words"}`

// answerVerdict is the judge's call on the quality of one answer, and only that. Whether the arm refused is never asked of the judge — it is read off the arm's own reply, where armAnswer.Declined is set, because the instruction already tells every arm to answer INSUFFICIENT when the rows cannot support an answer and a deterministic signal beats a model's opinion of one.
type answerVerdict struct {
	Verdict string `json:"verdict"`
	Why     string `json:"why"`
}

func (v answerVerdict) passed() bool { return strings.EqualFold(v.Verdict, "pass") }

// armAnswer is what one model said for one question, and the judge's call on it.
type armAnswer struct {
	Text string
	// Err is set when the ARM could not be reached, and JudgeErr when the judge could not. Both exclude the row from every rate rather than counting it as a bad answer.
	// JudgeErr is set when the judge could not be reached, which is not the same thing as a bad answer and must never be counted as one. Scoring an outage as a failure is how a quota running out turns into an apparent regression: the arms answered fine and the scorecard said they did not.
	JudgeErr string
	// Declined is set from the arm's own reply, not from the judge. See where it is assigned for why.
	Declined bool
	Latency  time.Duration
	Err      string
	V        answerVerdict
}

// track8Result is one question measured on both axes: whether the rows could answer it, and what each arm made of those rows.
type track8Result struct {
	question
	// JudgeErr is set when the sufficiency judge could not be reached for this question, which excludes it from the rates rather than scoring it a failure.
	JudgeErr   string
	Hits       []string
	Sufficient verdict
	Answers    map[string]armAnswer
}

// arm is one model family under test, named as it appears in the scorecard.
type arm struct {
	Name  string
	Brain brain.Brain
}

// capacityArms are the model families this machine can run at no API cost, each on its own subscription. Every one is invoked as a plain one-shot text call with its own tooling disabled where the CLI allows it, so what is being measured is the model reading the rows and nothing else.
// ponytail: agy has no flag to disable its tools, unlike claude's --tools "" and grok's --deny "*". The groundedness half of the answer judge is what catches an arm that answered from somewhere other than the rows, whatever the reason — which is a check worth having for every arm rather than a workaround for one.
func capacityArms() []arm {
	t := config.DefaultBrainTimeoutSeconds
	return []arm{
		{"claude-sonnet", brain.ClaudeCLI("claude", "sonnet", t)},
		{"grok", brain.GrokCLI("grok", t)},
		{"agy", brain.AgyCLI("agy", t)},
	}
}

// answerPrompt assembles what one arm sees: the instruction, the question, and the frozen rows.
func answerPrompt(q question, rows []string) string {
	var b strings.Builder
	b.WriteString(answerInstruction)
	if q.AskedAt != "" {
		fmt.Fprintf(&b, "\n\nThe user is asking this on %s. Read \"today\", \"yesterday\" and \"last week\" against that date, not against any other.", q.AskedAt)
	}
	fmt.Fprintf(&b, "\n\nQUESTION: %s\n\nMEMORY ROWS (%d):\n%s", q.Question, len(rows), rowsOrNone(rows))
	return b.String()
}

// runTrack8 retrieves once per question, freezes the rows, judges their sufficiency, then has every arm answer from those exact rows and judges each answer.
// Input: the search path, the judge, the arms, the questions, and where to freeze the rows. Output: one result per question.
func runTrack8(ctx context.Context, search track1Search, j *judge, arms []arm, qs []question, frozenPath string) []track8Result {
	results := make([]track8Result, 0, len(qs))
	for _, q := range qs {
		r := track8Result{question: q, Answers: map[string]armAnswer{}}

		rows, err := search(ctx, q)
		if err != nil {
			r.Sufficient = verdict{Verdict: "fail", Why: "retrieval errored"}
			results = append(results, r)
			fmt.Printf("  [%-3s] retrieval ERROR %v\n", q.ID, err)
			continue
		}
		r.Hits = rows

		if err := j.ask(ctx, track1Instruction, judgeMaterial(q, rows), &r.Sufficient); err != nil {
			r.JudgeErr = err.Error()
			r.Sufficient = verdict{Verdict: "", Why: "judge unreachable — excluded from every rate"}
		}
		fmt.Printf("  [%-3s] rows %2d  sufficient=%-4s\n", q.ID, len(rows), r.Sufficient.Verdict)

		for _, a := range arms {
			var ans armAnswer
			start := time.Now()
			text, err := a.Brain(ctx, answerPrompt(q, rows))
			ans.Latency = time.Since(start)
			if err != nil {
				// Not a bad answer: a missing CLI binary, a timeout, a rate limit. Scored as a failure it would report the arm as worse at reading retrieved context than it is — the same mistake the judge path made until JudgeErr existed.
				ans.Err = err.Error()
				ans.V = answerVerdict{Verdict: "", Why: "arm unreachable — excluded from every rate"}
				fmt.Printf("        %-14s ERROR %v\n", a.Name, err)
				r.Answers[a.Name] = ans
				continue
			}
			ans.Text = strings.TrimSpace(text)
			// Refusal is read off the arm's own words rather than asked of the judge. The instruction tells every arm to answer INSUFFICIENT when the rows cannot support an answer, so the signal is already deterministic — and asking the judge for it as a boolean alongside a verdict simply did not work: all three arms replied INSUFFICIENT to a question about a person who does not exist, and the judge reported declined=false for all three.
			ans.Declined = strings.Contains(strings.ToUpper(ans.Text), "INSUFFICIENT")
			material := judgeMaterial(q, rows) + "\n\nITS ANSWER:\n" + ans.Text
			if err := j.ask(ctx, answerJudgeInstruction, material, &ans.V); err != nil {
				ans.JudgeErr = err.Error()
				ans.V = answerVerdict{Verdict: "", Why: "judge unreachable — excluded from every rate"}
			}
			declined := ""
			if ans.Declined {
				declined = " declined"
			}
			mark := ans.V.Verdict
			if ans.JudgeErr != "" {
				mark = "SKIP"
			}
			fmt.Printf("        %-14s %-4s%-9s %5.1fs  %s\n", a.Name, mark, declined, ans.Latency.Seconds(), ans.V.Why)
			r.Answers[a.Name] = ans
		}
		results = append(results, r)
	}

	if err := freezeRows(frozenPath, results); err != nil {
		fmt.Printf("  (could not freeze rows: %v)\n", err)
	}
	return results
}

// freezeRows writes each question's retrieved rows and every arm's answer to disk, so the run can be re-judged, re-tabulated or given another arm without spending another round of retrieval and subscription calls. A one-shot study is worth keeping.
func freezeRows(path string, results []track8Result) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// track8Table renders two tables. The first crosses whether the rows were sufficient against what each arm did with them, which separates a retrieval failure from a model failure. The second scores each arm against what the question was designed to require, which is the only way to tell a correct refusal from a failure to answer.
func track8Table(results []track8Result, arms []arm) string {
	var b strings.Builder

	b.WriteString("\n| Arm | sufficient → answered well | sufficient → answered badly | insufficient → declined | insufficient → answered anyway |\n")
	b.WriteString("|---|---|---|---|---|\n")
	for _, a := range arms {
		var sufOK, sufBad, insufDeclined, insufAnswered int
		for _, r := range results {
			ans, ok := r.Answers[a.Name]
			// A judgement that never happened belongs in no cell. Left in, an outage reads as "the material was there and the arm failed to use it", which is the one cell that would send someone off to change the model.
			if !ok || ans.JudgeErr != "" || ans.Err != "" || r.JudgeErr != "" {
				continue
			}
			switch {
			case r.Sufficient.passed() && ans.V.passed():
				sufOK++
			case r.Sufficient.passed():
				sufBad++
			case ans.Declined:
				insufDeclined++
			default:
				insufAnswered++
			}
		}
		fmt.Fprintf(&b, "| %s | %d | **%d** | %d | **%d** |\n", a.Name, sufOK, sufBad, insufDeclined, insufAnswered)
	}
	b.WriteString("\n**sufficient → answered badly** is model failure: the material was there and the arm did not use it. **insufficient → answered anyway** is the arm producing an answer the rows could not support, which a single end-to-end score counts as a pass.\n")

	if !hasExpectations(results) {
		return b.String()
	}

	b.WriteString("\n| Arm | should answer | should refuse | should correct a false premise |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, a := range arms {
		var ansOK, ansN, refOK, refN, fpOK, fpN int
		for _, r := range results {
			got, ok := r.Answers[a.Name]
			if !ok || got.JudgeErr != "" || got.Err != "" {
				continue
			}
			switch r.Expect {
			case "answerable", "hard", "clarify":
				ansN++
				// Declining a question the set says is answerable is a failure however well it is worded.
				if got.V.passed() && !got.Declined {
					ansOK++
				}
			case "refuse":
				refN++
				if got.Declined {
					refOK++
				}
			case "false-premise":
				fpN++
				// A false premise wants correcting, not refusing: the rows do hold the answer, they just contradict the question.
				if got.V.passed() && !got.Declined {
					fpOK++
				}
			}
		}
		fmt.Fprintf(&b, "| %s | %d/%d | %d/%d | %d/%d |\n", a.Name, ansOK, ansN, refOK, refN, fpOK, fpN)
	}
	b.WriteString("\nThese three columns are what a single accuracy number cannot separate. An arm that answers everything scores well on the first and fails the second; one that hedges does the reverse.\n")
	return b.String()
}

// hasExpectations reports whether this question set says what each question should do, which the harvested log questions do not and the probe set does.
func hasExpectations(results []track8Result) bool {
	for _, r := range results {
		if r.Expect != "" {
			return true
		}
	}
	return false
}

// track8SufficientRate is how often retrieval put enough in front of the arms, counted without any arm's opinion of it. This is the retrieval axis on its own.
func track8SufficientRate(rs []track8Result) (passes, applicable int) {
	vs := make([]verdict, 0, len(rs))
	for _, r := range rs {
		if r.JudgeErr != "" {
			continue
		}
		vs = append(vs, r.Sufficient)
	}
	return rate(vs)
}

// judgeOutages counts the measurements that never happened — a judge that could not be reached or an arm that could not be run — so a run with an outage reports that plainly instead of reporting a worse system.
func judgeOutages(rs []track8Result) int {
	n := 0
	for _, r := range rs {
		if r.JudgeErr != "" {
			n++
		}
		for _, a := range r.Answers {
			if a.JudgeErr != "" || a.Err != "" {
				n++
			}
		}
	}
	return n
}

// track8ArmRate is how often one arm answered well, across every question it was asked. Declining for lack of material is excluded rather than counted against it: an arm that correctly says it cannot answer from thin rows is behaving properly, and scoring that as failure would reward guessing.
func track8ArmRate(rs []track8Result, name string) (passes, applicable int) {
	for _, r := range rs {
		a, ok := r.Answers[name]
		if !ok || a.JudgeErr != "" || a.Err != "" {
			continue
		}
		applicable++
		if a.V.passed() {
			passes++
		}
	}
	return passes, applicable
}

// writeTrack8Section writes the cross-tab and the per-question detail behind it.
func writeTrack8Section(b *strings.Builder, rs []track8Result, arms []arm) {
	b.WriteString("\n\n## Track 8 — context vs capacity\n\n")
	b.WriteString("Retrieval ran once per question and its rows were frozen; a judge scored whether those rows were sufficient, and separately each arm answered from those same rows and was scored on what it said. Crossing the two separates a question the material could never have answered from one the model failed to answer. The arms are three model families rather than one vendor's ladder, so a shared failure cannot be blamed on one lab being weak at this shape of question.\n")
	b.WriteString(track8Table(rs, arms))

	b.WriteString("\n| Q | Question | Expect | Rows | Sufficient |")
	for _, a := range arms {
		fmt.Fprintf(b, " %s |", a.Name)
	}
	b.WriteString("\n|---|---|---|---|---|")
	for range arms {
		b.WriteString("---|")
	}
	b.WriteString("\n")
	for _, r := range rs {
		fmt.Fprintf(b, "| %s | %s | %s | %d | %s |", r.ID, r.Question, orDash(r.Expect), len(r.Hits), r.Sufficient.Verdict)
		for _, a := range arms {
			ans := r.Answers[a.Name]
			mark := ans.V.Verdict
			if ans.Declined {
				mark += " (declined)"
			}
			fmt.Fprintf(b, " %s |", mark)
		}
		b.WriteString("\n")
	}
}

// orDash renders an empty expectation as a dash, for a question set that does not carry one.
func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
