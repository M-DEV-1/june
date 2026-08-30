// Package study is the distillation study pass: the teacher brain (Claude) reads the local/live models' real outputs — counterfactual replay side-by-sides from evals' track 5 and the dream package's overnight trace files — and writes durable correction lessons that a future prompt can embed. It only reads what those packages produce; it never writes into them.
package study

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"ora/internal/brain"
)

// Lesson is one durable, prompt-usable correction the teacher extracted from the material — a rule a future system prompt can embed, not a one-off critique of a single reply.
type Lesson struct {
	Title    string `json:"title"`
	Lesson   string `json:"lesson"`
	Evidence string `json:"evidence"`
}

// studyReply is the exact JSON shape the teacher is asked to answer in.
type studyReply struct {
	Lessons []Lesson `json:"lessons"`
	Summary string   `json:"summary"`
}

// Result is what one study pass produced, for the caller to log.
type Result struct {
	ReplaysRead  int // replay files successfully read
	TracesRead   int // trace files successfully read
	LinesSkipped int // trace JSONL lines that failed to parse as JSON
	LessonsAdded int // new lessons appended to lessons.md (titles already present there are excluded)
	ReportPath   string
	LessonsPath  string
}

// materialBudget caps the material folded into one study prompt. Most-recent files go in first; whatever doesn't fit is named in the prompt's inventory as truncated rather than silently dropped. A var, not a const, so a test can shrink it to exercise the truncation path without needing 40KB of fixture text.
var materialBudget = 40 * 1024

// block is one file's worth of material, tagged with where it came from for the inventory.
type block struct {
	source string
	text   string
}

// Study gathers the replay and trace material, asks the teacher brain once for durable correction lessons, and writes a per-run report plus an updated cumulative lessons file. Input: the teacher brain, the replay markdown paths and the trace JSONL paths (both listed by the caller, not globbed here, so tests can hand it fixtures), and the output directory. Output: a result summarising what was read and written, or an error if no material was found, the teacher call failed, its reply never parsed as JSON, or a write failed.
func Study(ctx context.Context, teach brain.Brain, replayPaths, tracePaths []string, outDir string) (Result, error) {
	var res Result
	var blocks []block

	for _, p := range replayPaths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue // a listed file that vanished between listing and reading is not fatal to the pass
		}
		blocks = append(blocks, block{source: p, text: string(b)})
		res.ReplaysRead++
	}

	for _, p := range tracePaths {
		text, skipped, err := readTrace(p)
		if err != nil {
			continue
		}
		if text != "" {
			blocks = append(blocks, block{source: p, text: text})
			res.TracesRead++
		}
		res.LinesSkipped += skipped
	}

	if len(blocks) == 0 {
		return res, fmt.Errorf("no material found: %d replay paths, %d trace paths given, none readable", len(replayPaths), len(tracePaths))
	}

	// Most recent first. Every file this package reads is named for the moment it covers — a replay's session start timestamp, a night's date — so a plain descending sort of the basename is chronological.
	sort.Slice(blocks, func(i, k int) bool {
		return filepath.Base(blocks[i].source) > filepath.Base(blocks[k].source)
	})

	included, truncated := fitBudget(blocks, materialBudget)

	reply, err := teach(ctx, composeStudyPrompt(included, truncated))
	if err != nil {
		return res, fmt.Errorf("teacher call: %w", err)
	}
	var parsed studyReply
	if err := parseStudyJSON(reply, &parsed); err != nil {
		return res, fmt.Errorf("teacher reply never parsed as JSON: %w", err)
	}

	reportPath, err := writeReport(outDir, included, truncated, parsed)
	if err != nil {
		return res, err
	}
	res.ReportPath = reportPath

	lessonsPath, added, err := appendLessons(outDir, parsed.Lessons)
	if err != nil {
		return res, err
	}
	res.LessonsPath = lessonsPath
	res.LessonsAdded = added

	return res, nil
}

// fitBudget keeps blocks — already ordered most-recent-first — until the byte budget runs out. Input: the ordered blocks and the budget. Output: the blocks that fit, and the source names of the ones that didn't.
func fitBudget(blocks []block, budget int) (kept []block, truncated []string) {
	used := 0
	for _, blk := range blocks {
		if used+len(blk.text) > budget {
			truncated = append(truncated, blk.source)
			continue
		}
		used += len(blk.text)
		kept = append(kept, blk)
	}
	return kept, truncated
}

// traceKeywords are the substrings (case-insensitive) a trace line's field name must contain to be pulled into the study material. This is the tolerant contract with whatever internal/dream's tracer actually logs: that package is being edited concurrently, its exact field names are not this package's to depend on, and this list is deliberately generous rather than pinned to today's shape.
var traceKeywords = []string{"prompt", "reply", "raw", "stage", "kind"}

// readTrace reads one night's trace JSONL file: each line unmarshals into a generic map, and every string field whose key looks like a prompt, a reply, a raw model output, or a stage label is kept. A line that isn't valid JSON, or that parses but has none of those fields, is skipped rather than failing the file — the count of unparsable lines is returned for the caller to report. Input: the file path. Output: the file's kept material rendered as text, how many lines failed to parse as JSON, and an error only if the file could not be opened at all.
func readTrace(path string) (string, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()

	var b strings.Builder
	skipped := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			skipped++
			continue
		}
		fields := extractFields(m)
		if len(fields) == 0 {
			continue
		}
		keys := make([]string, 0, len(fields))
		for k := range fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "%s: %s\n", k, fields[k])
		}
		b.WriteString("---\n")
	}
	return b.String(), skipped, nil
}

// extractFields picks out the string fields of one trace line whose key contains one of traceKeywords.
func extractFields(m map[string]any) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		lk := strings.ToLower(k)
		for _, kw := range traceKeywords {
			if strings.Contains(lk, kw) {
				if s, ok := v.(string); ok && s != "" {
					out[k] = s
				}
				break
			}
		}
	}
	return out
}

// studyRole is the fixed part of the study prompt: the teacher's role and the exact JSON it must answer in.
const studyRole = `You are the teacher model reviewing a weaker model's real outputs: side-by-side replays where you produced a counterfactual reply that a judge then compared against the weaker model's actual reply, and raw traces of the weaker model's own overnight reasoning calls.

Study the material below and extract durable, prompt-usable corrections — rules a future system prompt can embed, not one-off critiques of a single reply. Good: "when the user's words look garbled, say so instead of guessing." Bad: "the reply on turn 4 should have mentioned the FIDE proposal."

Reply with JSON only, in exactly this shape:
{"lessons":[{"title":"...","lesson":"...","evidence":"..."}],"summary":"..."}
Each lesson's "lesson" field must be 1-3 plain sentences a prompt could embed as-is. "evidence" names which material it came from.`

// composeStudyPrompt builds the one prompt for the teacher: the role and format instruction, an inventory of what material is included versus truncated for budget, and the included material itself, most recent first.
func composeStudyPrompt(included []block, truncated []string) string {
	var b strings.Builder
	b.WriteString(studyRole)
	b.WriteString("\n\n---\nMATERIAL INVENTORY (most recent first)\n")
	for _, blk := range included {
		fmt.Fprintf(&b, "- included: %s (%d bytes)\n", blk.source, len(blk.text))
	}
	for _, s := range truncated {
		fmt.Fprintf(&b, "- truncated (over the material budget, not sent): %s\n", s)
	}
	b.WriteString("---\n\n")
	for _, blk := range included {
		fmt.Fprintf(&b, "=== SOURCE: %s ===\n%s\n\n", blk.source, blk.text)
	}
	return b.String()
}

// parseStudyJSON decodes the teacher's reply into out, tolerating a markdown fence or leading/trailing prose around the JSON. This replicates internal/dream/dream.go's askJSON fence-stripping and outermost-JSON recovery, copied rather than imported since that package is being edited concurrently by another agent.
func parseStudyJSON(reply string, out any) error {
	body := stripFence(reply)
	if err := json.Unmarshal([]byte(body), out); err == nil {
		return nil
	}
	if sliced := outermostJSON(body); sliced != "" {
		if err := json.Unmarshal([]byte(sliced), out); err == nil {
			return nil
		}
	}
	return json.Unmarshal([]byte(body), out) // re-run to surface the real decode error in the message
}

// stripFence removes a markdown code fence around a JSON body — copied from internal/dream/dream.go's helper of the same name.
func stripFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	} else {
		return s
	}
	if i := strings.LastIndex(s, "```"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// outermostJSON slices s to the outermost JSON object or array it contains, or "" when it holds neither. Object is tried first here (unlike internal/dream's copy of this helper): dream's callers always want an array, but study's teacher reply is one object whose "lessons" field happens to be an array, so trying "[" first would slice out just that array and lose "summary".
func outermostJSON(s string) string {
	for _, pair := range [2][2]string{{"{", "}"}, {"[", "]"}} {
		start, end := strings.Index(s, pair[0]), strings.LastIndex(s, pair[1])
		if start >= 0 && end > start {
			return s[start : end+1]
		}
	}
	return ""
}

// writeReport renders one run's report as markdown: the material inventory, the teacher's summary, and its lessons in full. Input: the output directory, the material that was included and truncated, and the parsed reply. Output: the path written.
func writeReport(outDir string, included []block, truncated []string, reply studyReply) (string, error) {
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return "", err
	}
	path := filepath.Join(outDir, fmt.Sprintf("study-%s.md", time.Now().Format("2006-01-02")))

	var b strings.Builder
	fmt.Fprintf(&b, "# Distillation study — %s\n\n", time.Now().Format("2006-01-02 15:04 MST"))
	b.WriteString("## Material\n\n")
	for _, blk := range included {
		fmt.Fprintf(&b, "- %s (%d bytes)\n", blk.source, len(blk.text))
	}
	for _, s := range truncated {
		fmt.Fprintf(&b, "- %s (over the %d KB budget, not sent to the teacher)\n", s, materialBudget/1024)
	}
	b.WriteString("\n## Summary\n\n")
	b.WriteString(reply.Summary)
	b.WriteString("\n\n## Lessons\n\n")
	for _, l := range reply.Lessons {
		fmt.Fprintf(&b, "### %s\n\n%s\n\nEvidence: %s\n\n", l.Title, l.Lesson, l.Evidence)
	}
	return path, os.WriteFile(path, []byte(b.String()), 0644)
}

// appendLessons appends new lessons to the cumulative lessons.md, skipping any whose title already appears there. This file is what future dream and live prompts embed, so each lesson stays to its 1-3 plain sentences. Input: the output directory and the teacher's proposed lessons. Output: the file's path and how many were actually appended.
func appendLessons(outDir string, lessons []Lesson) (string, int, error) {
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return "", 0, err
	}
	path := filepath.Join(outDir, "lessons.md")
	existing, _ := os.ReadFile(path) // a missing file just means this is the first run

	var b strings.Builder
	added := 0
	for _, l := range lessons {
		heading := "## " + l.Title
		if strings.Contains(string(existing), heading+"\n") {
			continue
		}
		fmt.Fprintf(&b, "%s\n\n%s\n\n", heading, l.Lesson)
		added++
	}
	if added == 0 {
		return path, 0, nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return path, 0, err
	}
	defer f.Close()
	if _, err := f.WriteString(b.String()); err != nil {
		return path, 0, err
	}
	return path, added, nil
}
