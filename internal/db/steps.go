// This file renders an act run's steps into plain words: "looked at the screen", "clicked item 3 (Settings)", "typed into the box in front and pressed Enter". Two callers need exactly this rendering — the nightly procedures stage (internal/dream) turns it into a "How I did X" note, and the act-reference block (internal/agent) turns it into a line shown before a new screen ask — and until this file existed the same switch statement was kept as two hand-copied files, one per package, because internal/dream imports internal/agent and internal/agent cannot import internal/dream back. Both packages already import internal/db, so this is where the one copy lives.
package db

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"june/internal/act"
	"june/internal/util"
)

// stepLabelRunes caps an element label inside a rendered step, so one absurd button label cannot stretch the line.
const stepLabelRunes = 40

// RenderActSteps renders one run's tool calls as plain words, in call order, dropping every hop there is no plain wording for — a memory search or a file read, the non-screen tools a trace also carries. Input: the run's steps. Output: one short phrase per rendered step.
func RenderActSteps(steps []ActStep) []string {
	var out []string
	for _, s := range steps {
		if phrase := RenderActStep(s); phrase != "" {
			out = append(out, phrase)
		}
	}
	return out
}

// RenderActStep renders one tool call as plain words: "looked at the screen", "clicked item 3 (Settings)", "typed into the box in front and pressed Enter". Input: one step. Output: the phrase, or "" for a tool there is no wording for and for a numbered tool whose step names no readable item number.
func RenderActStep(s ActStep) string {
	switch s.Name {
	case "observe_screen":
		return "looked at the screen"
	case "show_marks":
		return "numbered what was on the screen"
	case "point_at":
		return renderActItemStep("pointed at item", s)
	case "click":
		return renderActItemStep("clicked item", s)
	case "scroll_to":
		return renderActItemStep("scrolled to item", s)
	case "type_text":
		// What was typed is never quoted: this is read back as a note embedded into the search index, or dropped straight into a prompt, and what the user dictated can be a passphrase or a private message. The step is worth a line anyway, so it says what was typed into — the box that had focus, which the step before it named — rather than what was typed. The text argument is dropped at the storage layer now (see AddActRun), but rows written before that still carry it, so it is ignored here rather than trusted to be gone.
		phrase := "typed into the box in front"
		if enter, _ := s.Args["enter"].(bool); enter {
			phrase += " and pressed Enter"
		}
		return phrase
	}
	return ""
}

// renderActItemStep renders one of the numbered screen tools as "<verb> N", with the element's label in brackets when a clean one can be read from the arguments or the head of the result. Input: the verb phrase, ending in the word "item", and the step. Output: the phrase, or "" when the step carries no readable item number.
func renderActItemStep(verb string, s ActStep) string {
	n, ok := renderActItemNumber(s.Args["n"])
	if !ok {
		return ""
	}
	phrase := fmt.Sprintf("%s %d", verb, n)
	if label := renderActLabel(s); label != "" {
		phrase += " (" + label + ")"
	}
	return phrase
}

// renderActItemNumber reads a step's "n" argument as an item number. Input: whatever JSON decoding left in the argument — a float64 from a round-tripped run, an int from a hand-built one, or a numeric string. Output: the number and true, or 0 and false when it is missing or unreadable.
func renderActItemNumber(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case float32:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(n))
		return i, err == nil
	}
	return 0, false
}

// renderActLabel finds what a rendered step may call the element it acted on: the "label" argument point_at carries, else the first quoted string in the head of the result, which is where click, scroll_to and point_at put the element's name. When the result says the element was a password box, a run of text or a box the user types into, neither is used — that name is the node's own contents, which is a password or a message and not a name at all — and the line carries the role word instead. Input: the step. Output: the label, the role word, or "" when there is none fit to print.
func renderActLabel(s ActStep) string {
	if role := renderActResultRole(s.Result); act.ContentRole(role) {
		return role
	}
	if arg, _ := s.Args["label"].(string); arg != "" {
		if label := renderActPlainLabel(arg); label != "" {
			return label
		}
	}
	return renderActPlainLabel(renderActQuoted(s.Result))
}

// renderActResultRole reads the element's role out of a screen tool's result, which names it between the item number and the quoted label: `clicked [4] password text "..." via press`. Input: a result head. Output: the role word, or "" when the result is not of that shape.
func renderActResultRole(result string) string {
	start := strings.Index(result, "] ")
	if start < 0 {
		return ""
	}
	rest := result[start+2:]
	end := strings.Index(rest, ` "`)
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}

// renderActQuoted returns the text between the first pair of double quotes in s, which for a screen tool's result is the element's own name and nothing else. Input: a result head. Output: the quoted text, or "" when the string holds no such pair.
func renderActQuoted(s string) string {
	start := strings.Index(s, `"`)
	if start < 0 {
		return ""
	}
	rest := s[start+1:]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return ""
	}
	return rest[:end]
}

// renderActPlainLabel keeps a label only if it is words a person would say, capped at stepLabelRunes. Input: a candidate label. Output: the label, or "" when it is empty, spans lines, or reads as machine detail — an accessibility bus name or object path, which must never reach a note or a prompt.
func renderActPlainLabel(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, "\n\r") {
		return ""
	}
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, ":1.") || strings.Contains(lower, "atspi") || strings.Contains(lower, "/org/") || strings.Contains(lower, "/accessible/") {
		return ""
	}
	return util.Runes(s, stepLabelRunes)
}
