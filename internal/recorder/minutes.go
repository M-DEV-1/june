package recorder

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"ora/internal/brain"
	"ora/internal/config"
	"ora/internal/db"
	"ora/internal/util"
)

// CoveredHeading is the minutes heading for the section that says what the meeting was about. app/src/next/task-about.tsx reads this same heading (as its own COVERED constant) to pull that section back out of the minutes for a task's detail pane, so the two must always read the same six words; TestCoveredHeadingMatchesFrontend checks that.
const CoveredHeading = "What the meeting covered"

// minutesInstruction tells the model what to make of the transcript. The microphone side is [me]; the system-audio side is [call], a single pooled label covering every remote voice, and the model's job is to put names to it from the screen context and from what was said.
var minutesInstruction = `You are writing the minutes of a meeting from an automatic transcript, for the person who recorded it. They will read this instead of remembering the meeting, so it is written for them first and about the meeting second.

The transcript labels every line with who spoke it:
  [me]      — the person whose computer recorded this, captured from their microphone. Always the same one person.
  [call:S1], [call:S2], ... — the other people on the call, captured from the computer's speakers and separated by voice. Each number is one person for the whole meeting: everything labelled [call:S3] is the same voice from start to finish. The numbers are arbitrary and carry no meaning beyond that.
  [call]    — a line the voice separation could not place. Treat it as an unknown speaker, not as a person in its own right and not as the same person as any other bare [call] line.

Because a numbered speaker is one consistent person, a name established anywhere applies everywhere that number appears. If [call:S4] is addressed as "Velmuth" once, then every [call:S4] line in the meeting is Velmuth, including the ones before the name was used.

The separation is good but not perfect. When people talk over each other the voices blend, so a line may land on the wrong number, and a person who spoke only briefly may be merged into someone else. Where a line's content plainly contradicts its number — a name, a role, an obvious continuation of someone else's sentence — trust the content and say the attribution is unclear rather than forcing it.

Your job includes working out WHO said what on the [call] side. Only these things name a person:
  1. The meeting app's own display of who is in the call: a participant tile, "X is presenting", a chat message with a sender name, the meeting title. Names there are real and correctly spelled.
  2. The transcript itself: someone introduces themselves, or one person addresses another by name.

These do NOT name anybody, and you must never take a name from them:
  - A username, commit author, pull request author, repository owner, file header, profile page, or issue assignee shown on screen. People read other people's code and pages all day; a name on a repository page is not a person in the room and is not whose computer this is.
  - A name written in a document, spreadsheet, ticket, email or web page that happened to be open.
  - A name that appears only once, in passing, with nothing tying it to a voice.

Rules for naming:
  - Attribute a line to a named person only when one of the two sources above actually points to it. Otherwise describe the role: "the interviewer", "the person presenting", "a second participant".
  - Never write "them", "the other side", or "the other participant" as if it were one person. Never write a speaker label like "[call:S2]" in the minutes themselves — name the person, or describe their role.
  - A name is a claim you must be able to point at evidence for. Knowing that [call:S2] and [call:S5] are two different people does not tell you either of their names.
  - Name everyone you can. Work at it: a name used once in the whole meeting, a person thanked at the end, a name in the meeting chat on screen, someone answering when addressed — all of these bind a name to a voice, and a name you can support is always better than a description.
  - When you genuinely cannot name someone, never leave them as a bare label. Describe them in a sentence or two that would let the reader work out who it was: what they were responsible for, what they talked about, who they answered to or were answering, when in the meeting they spoke, whether they were presenting. "The person who demoed the emissions upload and kept asking about custom emission factors" is useful. "A second participant" is not — it tells the reader nothing they could act on or recognise.
  - The transcript comes from speech recognition, so names in it may be misspelled. Where the screen context has the same name spelled properly, use that spelling.
  - A name or acronym that appears only in the transcript, with no match on screen and none in "About the person recording", is a guess by the recogniser. Write it once as heard and marked so, for example a contact (heard as "Oshveln") or the (heard as "PSP") task, and do not turn it into a fact about who someone is. On 2026-09-03 the recogniser turned Sorrek into Oshveln and PFP into PSP, and both were written into memory as true.
  - For the [me] speaker: this is always the same one person, the owner of this computer. What the user has told Ora about themselves is given below under "About the person recording" — if it names them, that is who [me] is, and it outranks anything on screen. Failing that, use a name they are addressed by in the call. Nothing else identifies whose machine this is — not the code on screen, not the accounts signed in. Otherwise call them "the person recording". Never write "[me]" in the minutes.

A section with nothing to report is a section you omit — heading and all — rather than fill in with a sentence that says so. Never write "Nothing recoverable", "Nothing was decided", "None beyond the above" or anything to that effect: a reader learns nothing from being told a section is empty that they would not already conclude from its absence. This applies to every section below, including a bullet inside "Your part" that would otherwise have nothing to fill it.

Write markdown with these sections, in this order.

# <the meeting's name>
Name the meeting as the people in it would name it — "Daily AI standup", "Value chain demo prep", "1:1 with Arun". Take it from the meeting app's window title, from the recurring shape of the conversation, or from how someone in the room refers to it. Do not write "Meeting minutes" as the heading, and do not invent a name grander than the meeting was. If you genuinely cannot tell, write the date and the attendees instead.

Directly under that heading, on its own line, repeat the name and when the meeting ran, in bold, with an em dash between them:
**<the meeting's name> — <day, date and times>**
This line is read back by the rest of the system to say which meeting a piece of owed work came from, so it is written even when it repeats the heading exactly.

## Your part
This section is about the [me] speaker only, and it is the reason the user opened this file. Three bullets, in this order, and each one only if it happened:
- **You said** — the update, position or argument [me] actually gave. Their own words compressed, not a description that they spoke.
- **Said to you** — what was said TO [me] in response: feedback, a correction, a question, a redirection, an agreement, a request. Name who it came from when you can. This is the part the user is most likely to have half-missed at the time, so do not skip it because it was brief.
- **You now owe** — what [me] committed to, or was asked for, with any date or deadline said out loud. One bullet per thing. If nothing was asked of them, omit this bullet rather than inventing a task or writing that nothing was asked.
If [me] barely spoke, say so in one line and move on. Never pad this section to make it look substantial.

## ` + CoveredHeading + `
The threads of the meeting, one bullet per thread, in the order they came up. A thread is a topic somebody opened and the room discussed — not every sentence. Attribute to named people where the evidence allows. This is where everything that was not about [me] goes.

## What others committed to
One bullet per person per commitment: who said they would do what, and by when if a date was said. Only things somebody actually agreed to, not things that were merely suggested. Omit this section entirely if nobody committed to anything.

## Decisions
What was actually settled, and who settled it. Omit this section entirely if nothing was decided.

## Action items
Every piece of owed work from this meeting in one list, the user's own included, so nothing agreed to is only recorded inside a paragraph. Repeat here what the sections above already said rather than leaving it out. Omit this section entirely if nothing was owed.
Two things belong here and nothing else: work the [me] speaker owes, and work a named person said out loud they would do. A topic that was discussed, a problem somebody described, a possibility floated, a "we should sit down and decide", or a state of play such as "left mid-check on the CHART-7 branch" is not owed work and gets no bullet. If nobody took the work on, there is no item at all: leave it out rather than recording it with the owner unknown.
Write each one on its own bullet in exactly this shape, with an em dash between the owner and the work:
- **Owner Name** — what they agreed to do, and by when if a date was said.
The owner is one of exactly two things, and nothing else — no role, no parenthetical, no "(recording)", and never "Owner unclear":
  - "Me", for anything the [me] speaker owes. Write it exactly, never their own name — for example "- **Me** — send the deck by Friday". "About the person recording" above names them; that name in the transcript is this person, so "<their name> will send the deck", "you'll send the deck" and "I'll send the deck" are all "Me".
  - One other person's name, for anything somebody else promised to do. Everyone but the [me] speaker keeps their own name.
Write the work so somebody who was not in the call can read it a week later: name the thing, the person or the file it is about, and what is being done to it. "Send Vexil the TCFD emissions file before the Q3 review" is an item; "send the file" is not.
The user's list is built from the "Me" items alone, so an item filed under the wrong owner either buries his own work or puts somebody else's on his plate.

## Attendees
Two lists, and every name goes in exactly one of them.
**In the meeting** — attendance requires that the person spoke or were named: either they have a line in the transcript, or someone named them during the call (addressed them, introduced them, thanked them). A participant tile, a "presenting" label, or a chat message on its own is not enough by itself to put someone here — it has to come with the person actually speaking or being named, not merely displayed. Use that evidence to decide the list, but do not write it out: one line per person, their name and a few words on their role in this call, nothing about where the name came from. Being talked ABOUT is not presence: an instruction, plan, or errand involving someone who never speaks and is never spoken to puts them under Mentioned, however often their name comes up. The recording person is one entry here once you know their name, written as "Their Name (recording)" — never also a separate "the person recording" bullet.
**Mentioned or on screen only** — names that came up in talk or on screen with nothing showing they were in the call: a commit author, a ticket assignee, an account name, someone discussed. Drop this list if there are none.
A name you can only hedge about ("referenced via screen context") goes in the second list, never the first.
A window title or chat header such as "Chat | A, B | Microsoft Teams" names the members of a chat thread, not the people on the call: a chat header is not attendance. Someone named only there goes in the second list unless they also spoke or were spoken to.
Every distinct voice gets an entry here even when it has no name: describe the person the way the naming rules above require, so an unnamed attendee is still a recognisable one. Do not write "unclear" and stop.

Be concise. Do not pad. Do not repeat the transcript back.`

// buildPrompt assembles the model input: the interleaved transcript plus the app-and-window timeline the tracker recorded while the meeting ran, which is what tells the model which app the call was in and what else was on screen.
func (r *Recorder) buildPrompt(ctx context.Context, transcript string, startedAt, stoppedAt time.Time) string {
	var b strings.Builder
	b.WriteString(minutesInstruction)
	fmt.Fprintf(&b, "\n\nThe meeting ran from %s to %s (%s).\n",
		startedAt.Format("Mon 2 Jan 2006 15:04"), stoppedAt.Format("15:04"), stoppedAt.Sub(startedAt).Round(time.Minute))

	if about := r.aboutTheUser(ctx); about != "" {
		b.WriteString("\nAbout the person recording — what the user has told Ora for certain about themselves and the people in their life. This is the [me] speaker, the same person in every meeting. It is background, not speech: never quote it as something someone said.\n")
		b.WriteString(about)
	}

	if timeline := r.desktopTimeline(ctx, startedAt, stoppedAt); timeline != "" {
		b.WriteString("\nScreen context — what was on the user's screens while the meeting ran, in order, as read out of the windows themselves. This is where participant names, presenter labels and chat senders come from. It is context, not speech: never quote it as something someone said.\n")
		b.WriteString(timeline)
	}

	b.WriteString("\nTranscript:\n")
	b.WriteString(transcript)
	return b.String()
}

// aboutTheUser renders the personal context store — the things the user has stated about themselves and the people in their life — as bullet lines. This is what identifies the [me] speaker: nothing on screen does, because a screen is full of other people's names.
// It reads personal context and nothing else. Notes are inferred, aged and rewritten by the memory compiler, and a guess about who the user is would name the wrong person in the minutes; every entry here came from the user's own mouth.
func (r *Recorder) aboutTheUser(ctx context.Context) string {
	entries, err := r.store.PersonalContext(ctx)
	if err != nil {
		slog.Warn("could not read personal context for meeting minutes", "error", err)
		return ""
	}
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "  - %s\n", strings.TrimSpace(e.Content))
	}
	return b.String()
}

// contextMargin widens the window of screen history a meeting's prompt is built from, at both ends. The meeting app's window is open before anyone joins and stays open after the call drops, and the names on it are as true a minute either side as they are during.
// It is not a guess: in the 2026-08-31 16:17 recording the other participant's name reached the database exactly once, at 16:28, one minute after the recording stopped — from the window switcher, listing the Teams window whose title read "Microsoft Teams (PWA) - Chat | Vexil Quorin". Queried on the recording's exact bounds, that episode was a minute out of reach and the minutes said the speaker could not be identified.
const contextMargin = 3 * time.Minute

// screenTextBudget caps how much of one episode's captured window text goes into the prompt. The names and presenter labels a meeting app shows sit near the front of what accessibility reads out, so the head of the text is the part worth keeping.
// meetingTextBudget is the same cap for the call's own window, which gets more room because it is the only episode whose text is the answer rather than the context: a six-person participant list with presenter labels and a few chat lines runs past 400 characters, and cutting it there cuts off the names.
const (
	screenTextBudget  = 400
	meetingTextBudget = 1500
)

// desktopTimeline renders the meeting window's episodes as "HH:MM app — window title" lines followed by the text read off that window, which is what carries participant names ("Ravix Dolmen (Presenting)", chat senders, the meeting title).
// Consecutive episodes with the same app, title and text collapse to one line, since the tracker samples every couple of seconds and most samples repeat.
func (r *Recorder) desktopTimeline(ctx context.Context, since, until time.Time) string {
	episodes, err := r.store.EpisodesInWindow(ctx, since.Add(-contextMargin), until.Add(contextMargin), episodeLimit)
	if err != nil {
		slog.Warn("could not read desktop context for meeting minutes", "error", err)
		return ""
	}
	var b strings.Builder
	for _, e := range sampleTimeline(dedupeEpisodes(episodes), timelineEntries) {
		text := e.ScreenText
		if text == "" {
			text = e.VisibleText
		}
		budget := screenTextBudget
		if isMeetingWindow(e.App, e.Title) {
			budget = meetingTextBudget
		}
		text = util.RunesEllipsis(util.OneLine(text), budget)

		// Episodes come back from SQLite in UTC and the line above states the meeting's own clock in local time, so the row is converted here to keep both on the clock the user was watching.
		fmt.Fprintf(&b, "  %s  %s — %s\n", e.CreatedAt.Local().Format("15:04"), e.App, e.Title)
		if text != "" {
			fmt.Fprintf(&b, "      on screen: %s\n", text)
		}
	}
	return b.String()
}

// dedupeEpisodes collapses runs of consecutive episodes showing the same app, title and text down to the first of them. The tracker samples every couple of seconds and most samples repeat, so this is where a meeting's rows shrink to the screens it actually visited — 27 rows became 7 screens on the 2026-08-31 standup.
// It runs before any cap, which is the whole point: capping first spends the budget on duplicates and then drops the end of the meeting.
func dedupeEpisodes(eps []db.Episode) []db.Episode {
	out := make([]db.Episode, 0, len(eps))
	var last string
	for _, e := range eps {
		text := e.ScreenText
		if text == "" {
			text = e.VisibleText
		}
		key := e.App + "\x00" + e.Title + "\x00" + text
		if key == last {
			continue
		}
		last = key
		out = append(out, e)
	}
	return out
}

// sampleTimeline keeps at most n entries, spread evenly across the whole slice rather than taken from its front. A meeting's decisions land at the end, so a timeline that runs out of room mid-way is worse than one that thins uniformly: the first and last entries are always kept.
func sampleTimeline(eps []db.Episode, n int) []db.Episode {
	if n <= 0 || len(eps) <= n {
		return eps
	}
	out := make([]db.Episode, 0, n)
	// Stride across the input so the samples land evenly, and the integer arithmetic keeps index 0 first and the final index last.
	for i := 0; i < n; i++ {
		out = append(out, eps[i*(len(eps)-1)/(n-1)])
	}
	return out
}

// defaultBrain is the default minutes seam: one one-shot text call to whichever backend the config's brain block names, which is the Gemini API unless the user has pointed it at a CLI they are already paying a subscription for.
// When the daemon has installed a brain through SetBrain, that one is used instead, so a meeting write-up is metered against the same shared daily quota as the rest of the daemon's unattended work.
// Without an installed brain, the config is read on each call rather than at construction, so changing provider takes effect on the next meeting instead of at the next daemon restart. On the Gemini API the model is then the one config.BackgroundModel names for the meeting-minutes job, so an unattended write-up spends the large per-day request allowance rather than the small one the user's own asks need.
// When Gemini answers 429 or 503 the write-up is handed to the fallback brain the daemon installed, so a spent daily allowance costs the meeting its speed rather than its minutes.
func (r *Recorder) defaultBrain(ctx context.Context, prompt string) (string, error) {
	primary := r.installedBrain()
	if primary == nil {
		cfg := config.BackgroundBrainConfig(config.LoadConfig().Brain, config.JobMeetingMinutes)
		primary = brain.FromConfig(cfg, r.apiKey)
	}
	return brain.WithCodexFallback(primary, r.minutesFallbackBrain())(ctx, prompt)
}

// SetBrain installs the brain meeting minutes are written with — in the daemon, the same brain the main brain and dream stages are metered against, so an unattended write-up spends the shared free-tier allowance rather than one of its own. Input: the brain to use; nil reverts to defaultBrain building its own per meeting.
func (r *Recorder) SetBrain(b brain.Brain) {
	r.mainBrainMu.Lock()
	defer r.mainBrainMu.Unlock()
	r.mainBrain = b
}

// installedBrain reads the installed brain under the lock, since the daemon may install it after a meeting is already in flight.
func (r *Recorder) installedBrain() brain.Brain {
	r.mainBrainMu.RLock()
	defer r.mainBrainMu.RUnlock()
	return r.mainBrain
}

// SetMinutesFallback installs the brain a failed minutes call hands over to — in the daemon, Codex under the user's ChatGPT login, which the recorder cannot build for itself because that needs an *agent.Agent. Input: the fallback brain; nil turns the hand-over off.
func (r *Recorder) SetMinutesFallback(b brain.Brain) {
	r.minutesFallbackMu.Lock()
	defer r.minutesFallbackMu.Unlock()
	r.minutesFallback = b
}

// minutesFallbackBrain reads the installed fallback under the lock, since the daemon may install it after a meeting is already in flight.
func (r *Recorder) minutesFallbackBrain() brain.Brain {
	r.minutesFallbackMu.RLock()
	defer r.minutesFallbackMu.RUnlock()
	return r.minutesFallback
}

// primingPrompt reads the desktop episodes recorded during the meeting and turns them into the initial prompt for whisper. It is best-effort: a store that cannot answer costs the transcript its spelling hints, not the transcript.
func (r *Recorder) primingPrompt(ctx context.Context, since, until time.Time) string {
	episodes, err := r.store.EpisodesInWindow(ctx, since.Add(-contextMargin), until.Add(contextMargin), episodeLimit)
	if err != nil {
		slog.Warn("could not read desktop context to prime whisper", "error", err)
		return ""
	}
	var people []string
	if entries, err := r.store.PersonalContext(ctx); err == nil {
		people = personNamesFromContext(entries)
	}
	return primingPromptFor(episodes, people)
}
