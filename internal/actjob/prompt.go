package actjob

import (
	"fmt"
	"strings"

	"ora/internal/act"
	"ora/internal/util"
)

// What one round's prompt is allowed to carry. The whole trail of a forty-step job lives in the checkpoint, not here: a round deciding which button to press next needs the plan, where the job has got to, what the screen looks like now and what the last few tools said, and nothing else. Sized so that the prompt of a job forty steps in is about the same as the prompt of a job one step in, which is what makes a long job affordable on a subscription.
const (
	// keptObservations is how many screen readings go in: the one just taken and the one before it, so the model can see what changed.
	keptObservations = 2
	// keptResults is how many tool results go in.
	keptResults = 3
	// keptAnswers is how many of the user's own answers to stuck questions go in, newest last.
	keptAnswers = 3
	// observationCap bounds one screen reading. A hundred-item listing is about 6,000 characters; this keeps the head of it, which is where the window line and the controls are.
	observationCap = 4000
	// resultCap bounds one tool result.
	resultCap = 700
	// answerCap bounds one of the user's answers.
	answerCap = 400
	// planCap bounds the plan the model wrote for itself.
	planCap = 1200
	// summaryCap bounds the rolling progress summary, which is meant to be two sentences.
	summaryCap = 600
)

// EstimateTokens is the rough token count of a prompt, at four characters to the token. It is an estimate on purpose: no provider reports what a prompt cost before it is sent, and the point of the number is to keep one round's prompt from growing with the job's length. Input: the prompt. Output: the estimate.
func EstimateTokens(s string) int { return len(s) / 4 }

// capRunes cuts a string to n runes.
func capRunes(s string, n int) string { return util.Runes(s, n) }

// pushCapped adds one entry to a rolling list, cutting the entry to cap runes and keeping only the newest keep entries. Input: the list, the new entry, how many to keep and the per-entry rune cap. Output: the new list.
func pushCapped(list []string, entry string, keep, cap int) []string {
	list = append(list, capRunes(entry, cap))
	if len(list) > keep {
		list = list[len(list)-keep:]
	}
	return list
}

// systemPrompt is the fixed instruction every round of every job opens with: one action per round, the change it must produce written down first, and the JSON to reply in. It takes no arguments, so the same bytes open every round of every job and a provider's prompt cache can match them.
var systemPrompt = `You are Ora, working through one task on the user's screen, one checked step at a time.

Each round you are shown the goal, your own plan, where you have got to, the last two readings of the screen and the last few tool results. Decide exactly ONE action, and write down the change it should produce before you take it. Something else checks whether that change actually came, so the check has to be a real, visible thing, not "the page loads".

Reply with one JSON object and nothing else:
{"plan":"...","next":"...","tool":"click","args":{"n":3},"expect":{"kind":"title_contains","value":"S16 E8"}}
  plan    the whole task in a sentence or two, written once on the first round and left alone after
  next    what this one action is, in plain words
  tool    one of: click, scroll_to, type_text, press_key, click_at, switch_window, open_app, open_url, observe_screen, look, point_at
  args    that tool's own arguments; click and scroll_to take {"n": <the number from the list>}
          press_key {"keys":"Enter"} is for the keys nothing in the list offers, and lands wherever the keyboard focus is, so click the field first
          click_at {"x":..,"y":..} is for what the list has no element or no working action for, and only in a round after a look, in that picture's own coordinates
  expect  the change the action should produce, as kind and value:
            ` + act.TitleContains + `  the window title will contain this text
            ` + act.ItemPresent + `    an item with this label will be showing
            ` + act.ItemAbsent + `     the item with this label will be gone
            ` + act.FieldHolds + `     the field being typed into will hold this text
            ` + act.ScreenChanged + `  the picture of the screen will differ from before, for what no list shows: a shell overlay or indicator, a video starting; the value says what you expect to see

When the goal is reached, reply {"done":true,"say":"..."} with one or two plain spoken sentences.
When you cannot get further without knowing something only the user knows, reply {"ask":"..."} with one plain question.
Never claim something worked because a tool returned; the check is what says it worked.
Write a check that can only become true after the action: the title of the page you are opening, an item that will appear, the item you are removing being gone. A check that was already true before you acted proves nothing, is not counted as a step that checked out, and the job cannot end on one, so if you are staying in a window that is already called what your check names, check for something on the screen that is about to change instead.
Work in the window already in front unless the goal needs another. A goal that needs an application (music, a chat, settings, a document) is done in that application: open_app {"app":"Spotify"} starts it or brings it forward, and open_url is only for a web page nothing installed is for. switch_window {"app":...} brings forward one already running, and observe_screen right after either. ` + "Never click anything that sends, pays, deletes or submits unless they have just said \"go\"."

// BuildPrompt renders one round's prompt. Input: the job as it stands. Output: the whole prompt, which stays about the same size whether the job is on its first step or its fortieth.
func BuildPrompt(j Job) string {
	var b strings.Builder
	b.WriteString(systemPrompt)
	// The machine is named right after the fixed head, and it does not change while the daemon runs, so a provider's prompt cache still matches across rounds.
	b.WriteString("\n\nTHE MACHINE\n")
	b.WriteString(util.DesktopLine())
	b.WriteString("\n\nGOAL\n")
	b.WriteString(j.Goal)
	if j.Window != "" {
		b.WriteString("\nThis is about the window: " + j.Window)
	}
	if j.Plan != "" {
		b.WriteString("\n\nPLAN\n")
		b.WriteString(capRunes(j.Plan, planCap))
	}
	// The answers come after the plan, not before it: the plan is written on the first round and never touched again, while an answer can arrive on any round, and putting the growing part first would move the plan to a new offset every time one did — which throws away the cached prefix that was holding it.
	if len(j.Answers) > 0 {
		b.WriteString("\n\nWHAT THE USER TOLD YOU WHEN YOU ASKED\n")
		for _, a := range lastN(j.Answers, keptAnswers) {
			b.WriteString("- " + capRunes(a, answerCap) + "\n")
		}
	}
	b.WriteString("\n\nWHERE YOU HAVE GOT TO\n")
	b.WriteString(progressLine(j))
	if j.Summary != "" {
		b.WriteString("\n" + capRunes(j.Summary, summaryCap))
	}
	if n := len(j.Steps); n > 0 {
		last := j.Steps[n-1]
		b.WriteString(fmt.Sprintf("\nThe last step was %s, expecting %s; it %s, and %s.", last.Tool, last.Expect.Describe(), outcomePhrase(last), capRunes(last.Why, resultCap)))
	}
	if j.Next != "" {
		b.WriteString("\nYou said the next thing to do was: " + j.Next)
	}
	b.WriteString("\n\nTHE SCREEN\n")
	for i, obs := range j.Observations {
		if i < len(j.Observations)-1 {
			b.WriteString("(an earlier reading)\n")
		} else {
			b.WriteString("(the reading just taken; its numbers are the ones click and scroll_to resolve against)\n")
		}
		b.WriteString(obs + "\n\n")
	}
	if len(j.Results) > 0 {
		b.WriteString("THE LAST FEW TOOL RESULTS\n")
		for _, res := range j.Results {
			b.WriteString("- " + res + "\n")
		}
	}
	b.WriteString("\n" + budgetLine(j))
	return b.String()
}

// outcomePhrase is how a step's outcome reads in a sentence. Input: the step. Output: "passed", "failed", or for a check that was already true before the action, a phrase saying so rather than one claiming the step worked.
func outcomePhrase(s Step) string {
	if s.Outcome == "pass" && s.HeldBefore {
		return "checked out on something that was already true before it"
	}
	return s.Outcome + "ed"
}

// progressLine says how far the job has got, in the two numbers that matter: steps taken and steps that checked out.
func progressLine(j Job) string {
	verified := 0
	for _, s := range j.Steps {
		if checkedOut(s) {
			verified++
		}
	}
	return fmt.Sprintf("%d steps taken, %d of which checked out.", len(j.Steps), verified)
}

// budgetLine tells the model what is left, so it can choose a shorter route rather than being cut off mid-task.
func budgetLine(j Job) string {
	return fmt.Sprintf("BUDGET: %d of %d steps left, about %d of %d input tokens left.",
		max(j.Budget.Steps-len(j.Steps), 0), j.Budget.Steps, max(j.Budget.InputTokens-j.Spend.Input, 0), j.Budget.InputTokens)
}

// SummaryPrompt asks for the two-sentence progress summary that replaces the trail in every later round. Input: the job. Output: the prompt, which carries the goal, the steps and the newest screen reading and nothing else.
func SummaryPrompt(j Job) string {
	var b strings.Builder
	b.WriteString("Rewrite where this screen task has got to, in at most two plain sentences. Say what has actually been achieved and what is still in the way. No preamble, no markdown, just the two sentences.\n\nGOAL\n")
	b.WriteString(j.Goal)
	b.WriteString("\n\nSTEPS SO FAR\n")
	for _, s := range lastN(j.Steps, keptSummarySteps) {
		b.WriteString(fmt.Sprintf("- %s, expecting %s: %s (%s)\n", s.Tool, s.Expect.Describe(), outcomePhrase(s), s.Why))
	}
	if len(j.Observations) > 0 {
		b.WriteString("\nTHE SCREEN NOW\n" + j.Observations[len(j.Observations)-1])
	}
	return b.String()
}

// keptSummarySteps is how many of the newest steps the summary rewrite is shown. Ten, because the summary it is rewriting already carries everything older.
const keptSummarySteps = 10

// lastN keeps the newest n entries of a slice.
func lastN[T any](items []T, n int) []T {
	if len(items) <= n {
		return items
	}
	return items[len(items)-n:]
}
