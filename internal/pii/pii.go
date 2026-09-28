// Package pii tears personally identifying information out of text before it goes to a remote model, and stitches it back into what comes back.
//
// It does not redact. Redaction is useless for a memory: "[REDACTED] said the venue is booked" can never be joined back to the person who said it, so the fact is stored and is worthless. Every substitution here is reversible, and every substitution resolves to an identity that is stable forever -- derived from the person's personal_context subject, never from where they happened to appear in this particular text. A token minted fresh per call ("A" today, "B" tomorrow) would destroy exactly the association this package exists to keep.
//
// The order of the passes is the order of how much is actually known:
//
//  1. A gazetteer of the people June already knows, keyed on the personal_context subjects. For these people a dictionary lookup is exact, reversible by construction, and needs no model at all.
//  2. Regex rules for structured PII -- email, phone, long digit runs -- which regex gets essentially right and a model gets wrong.
//  3. A seam for names June has never seen, with nothing behind it. See UnknownNames.
package pii

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"june/internal/db"
)

// Entity is one thing that was torn out of the text.
// Value is the exact text that was cut, which is what Stitch puts back. Subject is the personal_context subject for a known person and empty for structured PII, and it is the field that carries the association: it is the key the caller can file a memory under.
type Entity struct {
	Kind        string // "person", "email", "phone" or "account".
	Subject     string // personal_context subject, for a known person only.
	Value       string // the original text, as it appeared.
	Placeholder string // the token that stands in for it.
}

// Torn is the result of a tear: the text to send, and the record of who and what was in it.
type Torn struct {
	Text     string
	Entities []Entity // one per distinct entity, in the order they first appear in the text.
}

// Subjects returns the personal_context subjects of the known people who were in the torn text, in first-appearance order.
// This is how the association survives when the model answers with "she", "your colleague", or a paraphrase carrying no placeholder at all. Nothing can recover the name from that prose and this package does not try; what it guarantees is that the caller still holds the list of people the exchange was about, so the memory can be filed under them whatever the surface form came back as.
func (t Torn) Subjects() []string {
	var subjects []string
	for _, e := range t.Entities {
		if e.Subject != "" {
			subjects = append(subjects, e.Subject)
		}
	}
	return subjects
}

// Stitch puts the original values back wherever their placeholder appears in s. Input: text that came back from the model. Output: the same text with every placeholder this tear issued replaced by what it stood for.
// A placeholder the model invented, that this tear never issued, is left exactly as it is: guessing at what it was meant to mean would put a real person's name into a sentence that was never about them, which is worse than leaving an odd token on screen.
func (t Torn) Stitch(s string) string {
	if len(t.Entities) == 0 {
		return s
	}
	pairs := make([]string, 0, len(t.Entities)*2)
	for _, e := range t.Entities {
		pairs = append(pairs, e.Placeholder, e.Value)
	}
	return strings.NewReplacer(pairs...).Replace(s)
}

// UnknownNames finds spans of text that name a person the gazetteer has never heard of. It is the seam, and NoUnknownNames is deliberately the only implementation.
// A stranger's name carries no association to preserve: there is no personal_context row to file the memory under, so there is nothing a stable id could be derived from and nothing later retrieval could join it back to. Tearing it out would only cost the model context it needs to answer. When that changes -- when June starts minting rows for people it meets -- a model call or an NER pass is the upgrade path, and it plugs in here. This package holds no model of its own, which is a statement about what it is rather than a rule about what may be added: cgo is enabled on this project and nothing is stopped by the toolchain, and github.com/ebitengine/purego is already in the module graph if a native library is ever wanted without it. What a native runtime really costs is a shared library to ship inside the downloadable package.
type UnknownNames interface {
	Find(text string) []Span
}

// Span is a half-open byte range [Start, End) in the text handed to Find.
type Span struct {
	Start, End int
}

// NoUnknownNames finds nothing.
type NoUnknownNames struct{}

// Find returns no spans.
func (NoUnknownNames) Find(string) []Span { return nil }

// Tearer tears text against a fixed gazetteer of known people plus the structured-PII rules. Build one per set of personal_context entries; it holds no state across tears, so it is safe to reuse and safe to share.
type Tearer struct {
	// Unknown is consulted after the gazetteer and the rules. It is NoUnknownNames unless something sets it.
	Unknown UnknownNames

	aliases []alias
}

// alias is one spelling of one known person: the text to look for, lowercased, and the personal_context subject it resolves to.
type alias struct {
	text    string
	subject string
}

// New builds a Tearer from the personal_context entries, which is the whole gazetteer. Input: the rows db.Store.PersonalContext returns; entries that are not a person (the user's own "identity", the "preference..." subjects) are skipped, since the subject of those is not a name and no name can be derived from it.
// Each person contributes their full name, and -- only when the subject is a plain first-and-last name -- each of the two words on its own, so "Vexil" and "Quorin" both resolve. A longer subject like "vexil-quorin-mother" contributes only the full name: its last word is an ordinary English noun, and matching "mother" everywhere would attach the wrong person to half the sentences in the store.
func New(entries []db.PersonalEntry) *Tearer {
	t := &Tearer{Unknown: NoUnknownNames{}}
	for _, e := range entries {
		if !db.IsPersonSubject(e.Subject) {
			continue
		}
		name := strings.ToLower(db.PersonSubjectName(e.Subject))
		words := strings.Fields(name)
		if len(words) == 0 {
			continue
		}
		t.aliases = append(t.aliases, alias{text: name, subject: e.Subject})
		if len(words) == 2 {
			t.aliases = append(t.aliases, alias{text: words[0], subject: e.Subject},
				alias{text: words[1], subject: e.Subject})
		}
	}
	// A bare first name shared by two subjects ("vexil" is in both "vexil-quorin" and "vexil-quorin-mother") has to resolve the same way every time or the id is not stable. Shortest subject first, then alphabetical, makes that deterministic and picks the person whose name it plainly is over the longer qualified subject.
	sort.SliceStable(t.aliases, func(i, j int) bool {
		a, b := t.aliases[i], t.aliases[j]
		if a.text != b.text {
			return a.text < b.text
		}
		if n, m := strings.Count(a.subject, "-"), strings.Count(b.subject, "-"); n != m {
			return n < m
		}
		return a.subject < b.subject
	})
	return t
}

// The structured-PII rules. Each is kept tight enough not to eat ordinary numbers: a year and a time are too short for any of them, a price has no country code in front of it, and a bare mobile has to be exactly ten digits starting 6-9, which is the Indian mobile range.
var rules = []struct {
	kind string
	re   *regexp.Regexp
}{
	{"email", regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)},
	// A country code and then nine to sixteen digits in up to three groups: "+91 98765 43210", "+919876543210", "+1 555 1234567".
	{"phone", regexp.MustCompile(`\+\d{1,3}[ -]?\d{3,5}[ -]?\d{4,6}`)},
	// A bare Indian mobile, the one national format worth hardcoding here because it is the one the user's contacts are written in.
	{"phone", regexp.MustCompile(`\b[6-9]\d{9}\b`)},
	// A card written in the groups of four it is printed in.
	{"account", regexp.MustCompile(`\b\d{4}[ -]\d{4}[ -]\d{4}[ -]\d{4}\b`)},
	// An unbroken run this long is an account or card number; nothing in ordinary prose is twelve digits.
	{"account", regexp.MustCompile(`\b\d{12,19}\b`)},
}

// match is one candidate span before overlaps are resolved.
type match struct {
	start, end int
	kind       string
	subject    string
}

// Tear replaces every known person and every piece of structured PII in text with a stable placeholder. Input: the text about to be sent to a model. Output: the text to send and the record of what was taken out of it.
// Text with no PII in it comes back byte-identical.
func (t *Tearer) Tear(text string) Torn {
	matches := t.gazetteerMatches(text)
	for _, r := range rules {
		for _, loc := range r.re.FindAllStringIndex(text, -1) {
			matches = append(matches, match{start: loc[0], end: loc[1], kind: r.kind})
		}
	}
	for _, s := range t.Unknown.Find(text) {
		matches = append(matches, match{start: s.Start, end: s.End, kind: "person"})
	}
	if len(matches) == 0 {
		return Torn{Text: text}
	}

	// Longest match wins where two overlap: the full name beats the first name inside it, and the phone number beats the digit run inside the phone number. Earlier start first, then longer, then take greedily.
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].start != matches[j].start {
			return matches[i].start < matches[j].start
		}
		return matches[i].end > matches[j].end
	})

	var (
		out      strings.Builder
		entities []Entity
		byToken  = map[string]bool{}
		cursor   int
	)
	for _, m := range matches {
		if m.start < cursor {
			continue
		}
		value := text[m.start:m.end]
		e := Entity{
			Kind:        m.kind,
			Subject:     m.subject,
			Value:       value,
			Placeholder: placeholder(m.kind, identityKey(m.kind, m.subject, value)),
		}
		out.WriteString(text[cursor:m.start])
		out.WriteString(e.Placeholder)
		cursor = m.end
		// The same entity mentioned twice gets one record and one token, so two mentions of one person do not read as two people.
		if !byToken[e.Placeholder] {
			byToken[e.Placeholder] = true
			entities = append(entities, e)
		}
	}
	out.WriteString(text[cursor:])
	return Torn{Text: out.String(), Entities: entities}
}

// gazetteerMatches finds every alias in text on word boundaries, case-insensitively. Input: the original text. Output: one candidate per occurrence, unordered.
func (t *Tearer) gazetteerMatches(text string) []match {
	lower := strings.ToLower(text)
	var found []match
	for _, a := range t.aliases {
		for from := 0; from < len(lower); {
			i := strings.Index(lower[from:], a.text)
			if i < 0 {
				break
			}
			start := from + i
			end := start + len(a.text)
			if !wordChar(lower, start-1) && !wordChar(lower, end) {
				found = append(found, match{start: start, end: end, kind: "person", subject: a.subject})
			}
			from = start + 1
		}
	}
	return found
}

// wordChar reports whether the byte at i is a letter or digit, so a name is only a name when it is not glued to one. Input: the text and a byte index that may be out of range. Output: false at either edge of the text.
func wordChar(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return false
	}
	c := s[i]
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// identityKey is what the placeholder is derived from, and it is never the position in the text.
// For a known person it is the personal_context subject, so the same person is the same token in every call and across restarts for as long as that row exists. For structured PII it is the value itself, normalised, so one phone number written two ways ("+91 98765 43210" and "+919876543210") is one identity.
func identityKey(kind, subject, value string) string {
	switch kind {
	case "person":
		if subject != "" {
			return subject
		}
		return strings.ToLower(value)
	case "email":
		return strings.ToLower(value)
	default:
		var digits strings.Builder
		for _, r := range value {
			if r >= '0' && r <= '9' {
				digits.WriteRune(r)
			}
		}
		return digits.String()
	}
}

// placeholder builds the token that stands in for an entity: double square brackets around an uppercase kind and eight hex characters of the identity key's SHA-256.
// The form is chosen for what a model does with it. It is plain ASCII, so no tokeniser mangles it; "[[" and "]]" do not occur in ordinary English, so it cannot collide with prose and cannot be mistaken for something to rewrite; and PERSON_a1b2c3d4 reads to a model like an identifier in code, which is the kind of string models copy through verbatim instead of paraphrasing. The name itself is not in the token, so the tear actually hides something.
func placeholder(kind, key string) string {
	sum := sha256.Sum256([]byte(kind + ":" + key))
	return fmt.Sprintf("[[%s_%s]]", strings.ToUpper(kind), hex.EncodeToString(sum[:])[:8])
}
