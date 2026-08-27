// Package memory — observation.go defines the universal write shape for ambient captures: every stored moment is Content framed by Context.
//
// Kinds of memory get different handling later (moments decay, facts stick, periods live on the day tree) but they all share content+context: substance without framing is noise, framing without substance is empty.
package memory

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Kind classifies a memory item for retrieval policy.
type Kind string

const (
	// KindMoment is a point-in-time observation (episode). Ages with recency.
	KindMoment Kind = "moment"
	// KindFact is a durable user-stated note. No recency decay.
	KindFact Kind = "fact"
	// KindArc is an ongoing thread (subject + state).
	KindArc Kind = "arc"
	// KindPeriod is a day/session summary or digest on the temporal tree.
	KindPeriod Kind = "period"
)

// SignalKind is how the content was produced.
type SignalKind string

const (
	SignalVision    SignalKind = "vision"
	SignalA11y      SignalKind = "a11y"
	SignalTitleOnly SignalKind = "title_only"
	SignalMPRIS     SignalKind = "mpris"
)

// signalMaxWords caps moment content — better one short observation than a multi-KB accessibility dump that drowns FTS and embeddings.
const signalMaxWords = 120

// Context is the framing around content: where/when/what kind of memory.
type Context struct {
	App        string
	Title      string
	Domain     Domain
	Kind       Kind
	SignalKind SignalKind
}

// Observation is content + context — the only shape moments should take on the hot path (FTS, embeddings, tool output, inject).
type Observation struct {
	Content string
	Context Context
}

// Normalize builds an Observation from a raw capture: cleans chrome, caps length, classifies domain, and always fills context fields from app/title even when content ends up empty.
func Normalize(app, title, raw string) Observation {
	app = strings.TrimSpace(app)
	title = strings.TrimSpace(title)
	domain := Classify(app, title)

	content, sk := extractSignal(raw, title)
	return Observation{
		Content: content,
		Context: Context{
			App:        app,
			Title:      title,
			Domain:     domain,
			Kind:       KindMoment,
			SignalKind: sk,
		},
	}
}

// Document is what we store in episodes.screen_text, feed FTS, and embed.
// Context headers come first so keyword search on app/title works even when content is thin; content comes second so semantic search has the substance.
func (o Observation) Document() string {
	var parts []string
	head := strings.TrimSpace(o.Context.App + " · " + o.Context.Title)
	if head != "" && head != "·" {
		parts = append(parts, head)
	}
	if o.Context.Domain != DomainUnset {
		parts = append(parts, string(o.Context.Domain))
	}
	if c := strings.TrimSpace(o.Content); c != "" {
		parts = append(parts, c)
	}
	return strings.Join(parts, "\n")
}

// KindOf maps a DB/source string to Kind.
func KindOf(source string) Kind {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "episode", "moment":
		return KindMoment
	case "note", "fact":
		return KindFact
	case "thread", "arc":
		return KindArc
	case "summary", "digest", "period":
		return KindPeriod
	default:
		if source == "" {
			return KindMoment
		}
		return Kind(source)
	}
}

// FormatLine is the only way memory should enter the model: kind + domain + time + app/title context + excerpted content.
// Empty content yields a context-only line (still useful: "was in Netflix · Suits").
func FormatLine(kind Kind, domain Domain, at time.Time, app, title, content string, maxRunes int) string {
	if maxRunes <= 0 {
		maxRunes = 200
	}
	var meta []string
	meta = append(meta, string(kind))
	if domain != DomainUnset && domain != "" {
		meta = append(meta, string(domain))
	}
	if !at.IsZero() {
		meta = append(meta, at.Local().Format("2006-01-02 15:04"))
	}
	head := "[" + strings.Join(meta, " · ") + "]"

	ctx := strings.TrimSpace(app + " · " + title)
	if ctx == "·" {
		ctx = ""
	}

	body := collapseSpace(content)
	if maxRunes > 0 {
		body = truncateRunes(body, maxRunes)
	}

	switch {
	case ctx != "" && body != "":
		return head + " " + ctx + " — " + body
	case ctx != "":
		return head + " " + ctx
	case body != "":
		return head + " " + body
	default:
		return head
	}
}

// extractSignal cleans raw capture text into primary content.
func extractSignal(raw, title string) (string, SignalKind) {
	raw = stripControls(raw)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		t := strings.TrimSpace(title)
		if t == "" {
			return "", SignalTitleOnly
		}
		return t, SignalTitleOnly
	}

	// Prefer a trailing vision-like block when a11y chrome was concatenated ahead of DescribeScreen (daemon does text + "\n" + vtext).
	if preferred := preferVisionTail(raw); preferred != raw {
		raw = preferred
	}

	lines := strings.Split(raw, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || isChromeLine(line) {
			continue
		}
		kept = append(kept, line)
	}
	text := strings.Join(kept, "\n")
	text = collapseSpace(text)
	text = capWords(text, signalMaxWords)

	if text == "" {
		t := strings.TrimSpace(title)
		if t == "" {
			return "", SignalTitleOnly
		}
		return t, SignalTitleOnly
	}

	sk := SignalA11y
	if looksLikeVision(text) {
		sk = SignalVision
	}
	return text, sk
}

func preferVisionTail(raw string) string {
	// Split on blank lines; if the last block is short prose and earlier blocks are huge, prefer the last block (typical vision append).
	blocks := splitBlocks(raw)
	if len(blocks) < 2 {
		return raw
	}
	last := blocks[len(blocks)-1]
	lastWords := wordCount(last)
	headWords := wordCount(strings.Join(blocks[:len(blocks)-1], "\n"))
	if lastWords >= 8 && lastWords <= signalMaxWords && headWords > signalMaxWords && looksLikeVision(last) {
		return last
	}
	return raw
}

func splitBlocks(s string) []string {
	var blocks []string
	var cur []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) == "" {
			if len(cur) > 0 {
				blocks = append(blocks, strings.Join(cur, "\n"))
				cur = nil
			}
			continue
		}
		cur = append(cur, line)
	}
	if len(cur) > 0 {
		blocks = append(blocks, strings.Join(cur, "\n"))
	}
	return blocks
}

// looksLikeVision: short-ish prose, high letter ratio, few UI glyph runs.
func looksLikeVision(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	words := wordCount(s)
	if words < 5 || words > signalMaxWords+20 {
		return false
	}
	letters, glyphs, total := 0, 0, 0
	for _, r := range s {
		if unicode.IsSpace(r) {
			continue
		}
		total++
		switch {
		case unicode.IsLetter(r):
			letters++
		case isUIGlyph(r):
			glyphs++
		}
	}
	if total == 0 {
		return false
	}
	return float64(letters)/float64(total) >= 0.55 && float64(glyphs)/float64(total) < 0.05
}

// isChromeLine drops accessibility / TUI noise that is not language content.
func isChromeLine(line string) bool {
	letters, symbols, total := 0, 0, 0
	for _, r := range line {
		if unicode.IsSpace(r) {
			continue
		}
		total++
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			letters++
		case isUIGlyph(r):
			symbols++
		}
	}
	if total == 0 {
		return true
	}
	// Mostly glyphs / rules / waveforms
	if float64(symbols)/float64(total) > 0.4 {
		return true
	}
	if float64(letters)/float64(total) < 0.25 && total > 8 {
		return true
	}
	// Common a11y junk
	lower := strings.ToLower(line)
	for _, junk := range []string{
		"skip to main content", "skip to content", "cookie", "accept all",
	} {
		if lower == junk {
			return true
		}
	}
	return false
}

// isObjectChar reports whether r is an object replacement (U+FFFC) or unknown replacement (U+FFFD) character. AT-SPI reports every image, video, and icon as U+FFFC, so a screenful of thumbnails captures as nothing but these \u2014 they carry no meaning, but they tokenize and embed as if they did.
func isObjectChar(r rune) bool {
	return r == '\uFFFC' || r == '\uFFFD'
}

// StripObjectChars removes object replacement characters and drops any line they leave empty, returning "" when nothing but them was there. Input with none is returned trimmed and otherwise unchanged.
func StripObjectChars(s string) string {
	if !strings.ContainsFunc(s, isObjectChar) {
		return strings.TrimSpace(s)
	}
	var kept []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(strings.Map(func(r rune) rune {
			if isObjectChar(r) {
				return -1
			}
			return r
		}, line))
		if line != "" {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

func stripControls(s string) string {
	return strings.Map(func(r rune) rune {
		if isObjectChar(r) {
			return -1
		}
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

func collapseSpace(s string) string {
	fields := strings.Fields(s)
	return strings.Join(fields, " ")
}

func capWords(s string, max int) string {
	if max <= 0 {
		return s
	}
	words := strings.Fields(s)
	if len(words) <= max {
		return strings.Join(words, " ")
	}
	return strings.Join(words[:max], " ")
}

func wordCount(s string) int {
	return len(strings.Fields(s))
}

// isUIGlyph reports braille/box-drawing/block/geometric noise common in TUI and accessibility trees — not natural language content.
func isUIGlyph(r rune) bool {
	switch {
	case r >= 0x2800 && r <= 0x28FF: // Braille Patterns
		return true
	case r >= 0x2500 && r <= 0x257F: // Box Drawing
		return true
	case r >= 0x2580 && r <= 0x259F: // Block Elements
		return true
	case r >= 0x25A0 && r <= 0x25FF: // Geometric Shapes
		return true
	case r == '❯' || r == '•' || r == '·':
		return true
	default:
		return false
	}
}

func truncateRunes(s string, max int) string {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	if max < 1 {
		return ""
	}
	return string(runes[:max]) + "…"
}
