package actjob

import (
	"fmt"
	"strings"

	"june/internal/act"
	"june/internal/util"
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
	// referenceCap bounds the past-run block. Wide enough for the two runs and three lessons agent.ActReferenceBlock will render, and a backstop rather than the real limit, which is that renderer's own caps.
	referenceCap = 2000
	// summaryCap bounds the rolling progress summary, which is meant to be two sentences.
	summaryCap = 600
)

// EstimateTokens is the rough token count of a prompt, at four characters to the token. It is an estimate on purpose: no provider reports what a prompt cost before it is sent, and the point of the number is to keep one round's prompt from growing with the job's length. Input: the prompt. Output: the estimate.
func EstimateTokens(s string) int { return len(s) / 4 }

// pushCapped adds one entry to a rolling list, cutting the entry to cap runes and keeping only the newest keep entries. Input: the list, the new entry, how many to keep and the per-entry rune cap. Output: the new list.
func pushCapped(list []string, entry string, keep, cap int) []string {
	return util.LastN(append(list, util.Runes(entry, cap)), keep)
}

// systemPrompt is the fixed instruction every round of every job opens with: one action per round, the change it must produce written down first, and the JSON to reply in. It takes no arguments, so the same bytes open every round of every job and a provider's prompt cache can match them.
var systemPrompt = `You are June, working through one task on the user's screen, one checked step at a time.

Each round you are shown the goal, your own plan, where you have got to, the last two readings of the screen and the last few tool results. Decide exactly ONE action, and write down the change it should produce before you take it. Something else checks whether that change actually came, so the check has to be a real, visible thing, not "the page loads".

Reply with one JSON object and nothing else:
{"plan":"...","estimate":20,"next":"...","tool":"click","args":{"n":3},"expect":{"kind":"title_contains","value":"S16 E8"}}
To take several actions in one go, add "then": [{"tool":"click","args":{"n":7}},{"tool":"press_key","args":{"keys":"Enter"}}] — they run straight after your first one, in order, with nothing observed between them, and the one expect at the end checks where it all ended up.
  plan    the whole task in a sentence or two, written once on the first round and left alone after
  estimate on the first round only, roughly how many steps the whole task will take. Count everything: every look at the screen, every window you switch to, every check. You get twice what you say, so guess at what it would take going well rather than padding it. Nobody sets a limit for you, and if you spend it all without finishing you will be asked whether to carry on.
  next    what this one action is, in plain words
  tool    one of: click, scroll_to, type_text, press_key, click_at, switch_window, open_app, open_url, observe_screen, look, point_at, branch, query_memory, recall
  then    the rest of a burst, up to seven more actions after the first. Use it when you already know the whole sequence: WHAT YOU DID LAST TIME above is showing you a run that worked, or the list in front of you makes the next few taps certain, or the UI will not survive a pause (a menu that shuts when focus moves, a hover card, a toast). One round buys the whole burst, so a sequence you have done before costs one step instead of five, and the steps you save are yours to spend where the task is actually uncertain.
          Do not burst a guess. Nothing is read between the actions, so each one after the first lands on a screen you are predicting rather than looking at, and if the check at the end fails you will not know which of them went wrong — take the next ones one at a time and find out. A burst stops at its first refusal or error, so the rest of it does not land on a screen that has already gone somewhere else.
  args    that tool's own arguments; click and scroll_to take {"n": <the number from the list>}
          click also takes {"n":3,"then":[{"n":7},{"n":9}]} to tap several things in one go, with no round trip between them — for UI that does not survive one, a menu that shuts when focus moves, a hover card, a toast. Use it when you already know from the list where all the taps land; the whole burst is one step and one check
          branch {"task":"..."} is live web search, the only way to reach the web; use it to find a fact, a price or a page's address rather than browsing for it, and pass a URL it returns to open_url instead of typing one out
          query_memory {"query":"..."} and recall {"subject":"..."} read what you already know about this user — use them when the goal turns on something only they have told you
          these three change nothing on the screen, so they take no expect: reply with the tool and its args and leave expect out
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
	// Before the plan, because it is what the plan should be written against: the model reads what worked last time and then says what it will do this time. Sized by whoever rendered it (see agent.ActReferenceBlock), which already caps how many runs and lessons a block may carry.
	if j.Reference != "" {
		b.WriteString("\n\n")
		b.WriteString(util.Runes(j.Reference, referenceCap))
	}
	if j.Plan != "" {
		b.WriteString("\n\nPLAN\n")
		b.WriteString(util.Runes(j.Plan, planCap))
	}
	// The answers come after the plan, not before it: the plan is written on the first round and never touched again, while an answer can arrive on any round, and putting the growing part first would move the plan to a new offset every time one did — which throws away the cached prefix that was holding it.
	if len(j.Answers) > 0 {
		b.WriteString("\n\nWHAT THE USER TOLD YOU WHEN YOU ASKED\n")
		for _, a := range util.LastN(j.Answers, keptAnswers) {
			b.WriteString("- " + util.Runes(a, answerCap) + "\n")
		}
	}
	b.WriteString("\n\nWHERE YOU HAVE GOT TO\n")
	b.WriteString(progressLine(j))
	if j.Summary != "" {
		b.WriteString("\n" + util.Runes(j.Summary, summaryCap))
	}
	if n := len(j.Steps); n > 0 {
		last := j.Steps[n-1]
		b.WriteString(fmt.Sprintf("\nThe last step was %s, expecting %s; it %s, and %s.", last.Tool, last.Expect.Describe(), outcomePhrase(last), util.Runes(last.Why, resultCap)))
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
	for _, s := range util.LastN(j.Steps, keptSummarySteps) {
		b.WriteString(fmt.Sprintf("- %s, expecting %s: %s (%s)\n", s.Tool, s.Expect.Describe(), outcomePhrase(s), s.Why))
	}
	if len(j.Observations) > 0 {
		b.WriteString("\nTHE SCREEN NOW\n" + j.Observations[len(j.Observations)-1])
	}
	return b.String()
}

// keptSummarySteps is how many of the newest steps the summary rewrite is shown. Ten, because the summary it is rewriting already carries everything older.
const keptSummarySteps = 10
