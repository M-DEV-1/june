//go:build taskeval && linux

package taskeval

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"june/taskeval/fixture"
)

// memoryTask is one question template. Scripted mode grades what the tools returned to the model (toolsHave) and the answer (answerOK); live mode grades the answer alone.
type memoryTask struct {
	name      string
	question  func(w fixture.World) string
	vars      func(w fixture.World) map[string]string
	toolsHave func(w fixture.World, tools string) error
	answerOK  func(w fixture.World, answer string) error
}

var memoryTasks = []memoryTask{
	{
		name:     "owner",
		question: func(w fixture.World) string { return fmt.Sprintf("Who owns %s?", w.Projects[w.OwnerQ].Name) },
		vars: func(w fixture.World) map[string]string {
			p := w.Projects[w.OwnerQ]
			return map[string]string{"project": p.Name, "owner": p.Owner}
		},
		toolsHave: func(w fixture.World, tools string) error {
			p := w.Projects[w.OwnerQ]
			return lineWith(tools, p.Owner, p.Name)
		},
		answerOK: func(w fixture.World, answer string) error {
			owner := w.Projects[w.OwnerQ].Owner
			if !strings.Contains(answer, owner) {
				return fmt.Errorf("answer does not name %s", owner)
			}
			for i, p := range w.Projects {
				if i != w.OwnerQ && strings.Contains(answer, p.Owner) {
					return fmt.Errorf("answer names %s, who owns a different project", p.Owner)
				}
			}
			return nil
		},
	},
	{
		// The person asked about has a seeded ownership row and no phone number anywhere in the store.
		name: "unseeded",
		question: func(w fixture.World) string {
			return fmt.Sprintf("What is %s's phone number?", w.Projects[w.PhoneQ].Owner)
		},
		vars: func(w fixture.World) map[string]string {
			p := w.Projects[w.PhoneQ]
			return map[string]string{"person": p.Owner, "project": p.Name}
		},
		toolsHave: func(w fixture.World, tools string) error {
			p := w.Projects[w.PhoneQ]
			return lineWith(tools, p.Owner, p.Name)
		},
		answerOK: func(w fixture.World, answer string) error {
			if m := phoneLike.FindString(answer); m != "" {
				return fmt.Errorf("answer gives a phone number, %q, that is not in memory", m)
			}
			return nil
		},
	},
	{
		name: "attribution",
		question: func(w fixture.World) string {
			return fmt.Sprintf("Who said the %s was moved?", w.Claims[w.ClaimQ].Topic)
		},
		vars: func(w fixture.World) map[string]string {
			c := w.Claims[w.ClaimQ]
			return map[string]string{"topic": c.Topic, "speaker": c.Speaker, "date": c.Date}
		},
		toolsHave: func(w fixture.World, tools string) error {
			c := w.Claims[w.ClaimQ]
			return lineWith(tools, c.Speaker, c.Topic, c.Date)
		},
		answerOK: func(w fixture.World, answer string) error {
			c, other := w.Claims[w.ClaimQ], w.Claims[1-w.ClaimQ]
			if !strings.Contains(answer, c.Speaker) {
				return fmt.Errorf("answer does not name %s", c.Speaker)
			}
			if strings.Contains(answer, other.Speaker) {
				return fmt.Errorf("answer names %s, who said something else", other.Speaker)
			}
			return nil
		},
	},
}

// phoneLike matches seven or more digits in a row, or digit groups laid out as 3-4 or 3-3-4 with spaces, dots or dashes. An ISO date is 4-2-2 and does not match.
var phoneLike = regexp.MustCompile(`\d{7,}|\b\d{3}[-. ]\d{3}[-. ]\d{4}\b|\b\d{3}[-. ]\d{4}\b`)

// lineWith returns nil when some line of text contains every one of parts, so a name is only credited when it comes back in the same row as the fact it belongs to.
func lineWith(text string, parts ...string) error {
	for _, line := range strings.Split(text, "\n") {
		found := true
		for _, p := range parts {
			found = found && strings.Contains(line, p)
		}
		if found {
			return nil
		}
	}
	return fmt.Errorf("no tool result line has all of %q", parts)
}

// TestMemory runs every memory task against its own daemon seeded with each seed's world, and appends one line per task to out/results.jsonl. A task gets a fresh daemon because the daemon keeps running an ask after the harness gives up on it, and that ask would otherwise share the model and the memory figures with the next task.
func TestMemory(t *testing.T) {
	for _, s := range seeds(t) {
		world := fixture.Build(s.Seed)
		t.Run(fmt.Sprintf("seed=%d", s.Seed), func(t *testing.T) {
			for _, task := range memoryTasks {
				t.Run(task.name, func(t *testing.T) {
					d := startDaemon(t, world)
					run, err := runTask(t, d, task.name, task.question(world), task.vars(world))
					if err == nil && !live {
						err = task.toolsHave(world, run.Tools)
					}
					if err == nil {
						err = task.answerOK(world, run.Answer)
					}
					rss, _ := d.peakRSSKB()
					r := result{Suite: "memory", Task: task.name, Seed: s.Seed, Split: s.Split, Pass: err == nil, Steps: run.Steps, WallMS: run.Wall.Milliseconds(), PeakRSSKB: rss}
					if err != nil {
						r.Note = err.Error()
						t.Errorf("%v\nanswer: %q\ntool results: %q", err, run.Answer, run.Tools)
					}
					record(t, r)
				})
			}
		})
	}
}
