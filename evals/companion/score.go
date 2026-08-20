package companion

import (
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"ora/evals/dataset"
	"ora/internal/agent"
)

// Score is how a single channel did on one question, split so we can tell "knew it in thought but didn't say it" from "never found it", and whether the answer treated a weeks-old hit as "recent".
type Score struct {
	AnswerHit     bool
	ThoughtHit    bool
	ToolResultHit bool
	HandshakeHit  bool
	RetrieveHit   bool
	TimeOK        bool
	PresentSlip   bool
	RecencyNote   string
	Missing       []string
	ToolNames     []string
}

// Grade reports which of the expected needles landed in the spoken/written answer, the thoughts, the tool results, the frozen handshake, and the per-turn inject. Recency is scored against the question's Horizon (stamped from sqlite, not wall-clock). Input: question + the trace from AskText/AskVoice. Output: Score.
func Grade(q dataset.Question, tr agent.TurnTrace) Score {
	s := Score{
		AnswerHit:     containsAllFold(tr.Answer, q.Expect),
		ThoughtHit:    containsAllFold(strings.Join(tr.Thoughts, "\n"), q.Expect),
		ToolResultHit: containsAllFold(joinToolResults(tr), q.Expect),
		HandshakeHit:  containsAllFold(strings.Join(tr.Handshake, "\n"), q.Expect),
		RetrieveHit:   containsAllFold(strings.Join(tr.Injected, "\n"), q.Expect),
	}
	for _, hop := range tr.ToolHops {
		s.ToolNames = append(s.ToolNames, hop.Name)
	}
	if !s.AnswerHit {
		s.Missing = missingFold(tr.Answer, q.Expect)
	}
	s.TimeOK, s.PresentSlip, s.RecencyNote = gradeRecency(q, tr)
	return s
}

func gradeRecency(q dataset.Question, tr agent.TurnTrace) (ok, slip bool, note string) {
	answer := tr.Answer
	staleHit := oldestAge(joinToolResults(tr) + "\n" + strings.Join(tr.Handshake, "\n") + "\n" + strings.Join(tr.Injected, "\n"))
	timed := hasTimeCue(answer)
	present := hasPresentCue(answer)

	switch q.Horizon {
	case dataset.HorizonDurable:
		if staleHit >= 7*24*time.Hour && present && !timed {
			return true, true, "durable fact is fine in present tense, but a " + ageLabelDur(staleHit) + " hit was called recent"
		}
		return true, false, "durable fact — present tense is fine"
	case dataset.HorizonStale:
		if timed {
			return true, present && !timed, "stale memory dated in the answer"
		}
		msg := "stale memory (" + q.AgeLabel + ") needs a when — last week, Aug 11, 8d ago — not 'recent'"
		if q.AgeLabel == "" {
			msg = "stale memory needs a when — last week, a date, Nd ago — not 'recent'"
		}
		return false, present, msg
	default:
		if staleHit >= 7*24*time.Hour && present && !timed {
			return true, true, "current question mixed in a " + ageLabelDur(staleHit) + " hit as if it were now"
		}
		if q.AgeLabel != "" {
			return true, false, q.Horizon + " · " + q.AgeLabel
		}
		return true, false, q.Horizon
	}
}

var (
	ageDay  = regexp.MustCompile(`\((\d+)d ago\)`)
	ageHour = regexp.MustCompile(`\((\d+)h ago\)`)
	timeCue = regexp.MustCompile(`(?i)\b(yesterday|last week|last month|days? ago|\d+d ago|\d+h ago|january|february|march|april|june|july|august|september|october|november|december|jan\b|feb\b|mar\b|apr\b|jun\b|jul\b|aug\b|sep\b|oct\b|nov\b|dec\b|\d{1,2}(st|nd|rd|th)? of |20\d{2})\b`)
)

var presentCues = []string{
	"right now", "currently", "just now", "just looking",
	"this morning", "recently", "recent ",
	"you've been", "you have been", "you were just",
}

func oldestAge(blob string) time.Duration {
	var oldest time.Duration
	for _, m := range ageDay.FindAllStringSubmatch(blob, -1) {
		n, _ := strconv.Atoi(m[1])
		d := time.Duration(n) * 24 * time.Hour
		if d > oldest {
			oldest = d
		}
	}
	for _, m := range ageHour.FindAllStringSubmatch(blob, -1) {
		n, _ := strconv.Atoi(m[1])
		d := time.Duration(n) * time.Hour
		if d > oldest {
			oldest = d
		}
	}
	return oldest
}

func hasTimeCue(s string) bool {
	return timeCue.MatchString(s)
}

func hasPresentCue(s string) bool {
	lower := fold(s)
	for _, cue := range presentCues {
		if strings.Contains(lower, cue) {
			return true
		}
	}
	return false
}

func ageLabelDur(d time.Duration) string {
	days := int(d.Hours() / 24)
	if days >= 1 {
		return strconv.Itoa(days) + "d ago"
	}
	hours := int(d.Hours())
	if hours >= 1 {
		return strconv.Itoa(hours) + "h ago"
	}
	return d.Round(time.Minute).String()
}

func joinToolResults(tr agent.TurnTrace) string {
	var b strings.Builder
	for _, hop := range tr.ToolHops {
		b.WriteString(hop.Name)
		b.WriteByte(' ')
		b.WriteString(hop.Result)
		b.WriteByte('\n')
	}
	return b.String()
}

func containsAllFold(haystack string, needles []string) bool {
	if len(needles) == 0 {
		return false
	}
	lower := fold(haystack)
	for _, n := range needles {
		if n == "" {
			continue
		}
		if !strings.Contains(lower, fold(n)) {
			return false
		}
	}
	return true
}

func missingFold(haystack string, needles []string) []string {
	lower := fold(haystack)
	var missing []string
	for _, n := range needles {
		if n == "" {
			continue
		}
		if !strings.Contains(lower, fold(n)) {
			missing = append(missing, n)
		}
	}
	return missing
}

func fold(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		return unicode.ToLower(r)
	}, s)
}
