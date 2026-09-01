package recorder

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"ora/internal/brain"
	"ora/internal/config"
)

// minutesInstruction tells the model what to make of the transcript. The microphone side is [me]; the system-audio side is [call], a single pooled label covering every remote voice, and the model's job is to put names to it from the screen context and from what was said.
const minutesInstruction = `You are writing the minutes of a meeting from an automatic transcript, for the person who recorded it. They will read this instead of remembering the meeting, so it is written for them first and about the meeting second.

The transcript labels every line with who spoke it:
  [me]      — the person whose computer recorded this, captured from their microphone. Always the same one person.
  [call:S1], [call:S2], ... — the other people on the call, captured from the computer's speakers and separated by voice. Each number is one person for the whole meeting: everything labelled [call:S3] is the same voice from start to finish. The numbers are arbitrary and carry no meaning beyond that.
  [call]    — a line the voice separation could not place. Treat it as an unknown speaker, not as a person in its own right and not as the same person as any other bare [call] line.

Because a numbered speaker is one consistent person, a name established anywhere applies everywhere that number appears. If [call:S4] is addressed as "Vishal" once, then every [call:S4] line in the meeting is Vishal, including the ones before the name was used.

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
  - For the [me] speaker: this is always the same one person, the owner of this computer. What the user has told Ora about themselves is given below under "About the person recording" — if it names them, that is who [me] is, and it outranks anything on screen. Failing that, use a name they are addressed by in the call. Nothing else identifies whose machine this is — not the code on screen, not the accounts signed in. Otherwise call them "the person recording". Never write "[me]" in the minutes.

Write markdown with these sections, in this order.

# <the meeting's name>
Name the meeting as the people in it would name it — "Daily AI standup", "Value chain demo prep", "1:1 with Arun". Take it from the meeting app's window title, from the recurring shape of the conversation, or from how someone in the room refers to it. Do not write "Meeting minutes" as the heading, and do not invent a name grander than the meeting was. If you genuinely cannot tell, write the date and the attendees instead.

Directly under that heading, on its own line, repeat the name and when the meeting ran, in bold, with an em dash between them:
**<the meeting's name> — <day, date and times>**
This line is read back by the rest of the system to say which meeting a piece of owed work came from, so it is written even when it repeats the heading exactly.

## Your part
This section is about the [me] speaker only, and it is the reason the user opened this file. Three bullets, in this order, and each one only if it happened:
- **You said** — the update, position or argument [me] actually gave. Their own words compressed, not a description that they spoke.
- **You heard back** — what was said TO [me] in response: feedback, a correction, a question, a redirection, an agreement, a request. Name who it came from when you can. This is the part the user is most likely to have half-missed at the time, so do not skip it because it was brief.
- **You now owe** — what [me] committed to, or was asked for, with any date or deadline said out loud. One bullet per thing. If nothing was asked of them, write that plainly rather than inventing a task.
If [me] barely spoke, say so in one line and move on. Never pad this section to make it look substantial.

## What the meeting covered
The threads of the meeting, one bullet per thread, in the order they came up. A thread is a topic somebody opened and the room discussed — not every sentence. Attribute to named people where the evidence allows. This is where everything that was not about [me] goes.

## What others committed to
One bullet per person per commitment: who said they would do what, and by when if a date was said. Only things somebody actually agreed to, not things that were merely suggested. If nobody committed to anything, say so.

## Decisions
What was actually settled, and who settled it. If nothing was decided, say so in one line.

## Action items
Every piece of owed work from this meeting in one list, the user's own included, so nothing agreed to is only recorded inside a paragraph. Repeat here what the sections above already said rather than leaving it out.
Write each one on its own bullet in exactly this shape, with an em dash between the owner and the work:
- **Owner Name** — what they agreed to do, and by when if a date was said.
The owner is one person's name and nothing else: no role, no parenthetical, no "(recording)". Write the user by their own name like anybody else. When nobody was named, write "Owner unclear" as the owner rather than dropping the item.

## Attendees
Two lists, and every name goes in exactly one of them.
**In the meeting** — only people with evidence they were in the call: they spoke, they were spoken TO by name during it, or the meeting app or a shared working surface showed them taking part (participant tile, "presenting" label, a message they sent in the meeting's chat or shared whiteboard while it ran). Say which of those it was. Being talked ABOUT is not presence: an instruction, plan, or errand involving someone who never speaks and is never spoken to puts them under Mentioned, however often their name comes up. The recording person is one entry here once you know their name, written as "Their Name (recording)" — never also a separate "the person recording" bullet.
**Mentioned or on screen only** — names that came up in talk or on screen with nothing showing they were in the call: a commit author, a ticket assignee, an account name, someone discussed. Drop this list if there are none.
A name you can only hedge about ("referenced via screen context") goes in the second list, never the first.
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
// It is not a guess: in the 2026-08-31 16:17 recording the other participant's name reached the database exactly once, at 16:28, one minute after the recording stopped — from the window switcher, listing the Teams window whose title read "Microsoft Teams (PWA) - Chat | Trupti Hosmani". Queried on the recording's exact bounds, that episode was a minute out of reach and the minutes said the speaker could not be identified.
const contextMargin = 3 * time.Minute

// screenTextBudget caps how much of one episode's captured window text goes into the prompt. The names and presenter labels a meeting app shows sit near the front of what accessibility reads out, so the head of the text is the part worth keeping.
// meetingTextBudget is the same cap for the call's own window, which gets more room because it is the only episode whose text is the answer rather than the context: a six-person participant list with presenter labels and a few chat lines runs past 400 characters, and cutting it there cuts off the names.
const (
	screenTextBudget  = 400
	meetingTextBudget = 1500
)

// desktopTimeline renders the meeting window's episodes as "HH:MM app — window title" lines followed by the text read off that window, which is what carries participant names ("Arjun Goel (Presenting)", chat senders, the meeting title).
// Consecutive episodes with the same app, title and text collapse to one line, since the tracker samples every couple of seconds and most samples repeat.
func (r *Recorder) desktopTimeline(ctx context.Context, since, until time.Time) string {
	episodes, err := r.store.EpisodesInWindow(ctx, since.Add(-contextMargin), until.Add(contextMargin), episodeLimit)
	if err != nil {
		slog.Warn("could not read desktop context for meeting minutes", "error", err)
		return ""
	}
	var b strings.Builder
	var last string
	for _, e := range episodes {
		text := e.ScreenText
		if text == "" {
			text = e.VisibleText
		}
		budget := screenTextBudget
		if isMeetingWindow(e.App, e.Title) {
			budget = meetingTextBudget
		}
		text = truncate(strings.Join(strings.Fields(text), " "), budget)

		key := e.App + "\x00" + e.Title + "\x00" + text
		if key == last {
			continue
		}
		last = key

		fmt.Fprintf(&b, "  %s  %s — %s\n", e.CreatedAt.Format("15:04"), e.App, e.Title)
		if text != "" {
			fmt.Fprintf(&b, "      on screen: %s\n", text)
		}
	}
	return b.String()
}

// truncate shortens s to at most n runes, marking that it was cut.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// defaultBrain is the default minutes seam: one one-shot text call to whichever backend the config's brain block names, which is the Gemini API unless the user has pointed it at a CLI they are already paying a subscription for.
// The config is read on each call rather than at construction, so changing provider takes effect on the next meeting instead of at the next daemon restart.
func (r *Recorder) defaultBrain(ctx context.Context, prompt string) (string, error) {
	return brain.FromConfig(config.LoadConfig().Brain, r.apiKey)(ctx, prompt)
}

// primingPrompt reads the desktop episodes recorded during the meeting and turns them into the initial prompt for whisper. It is best-effort: a store that cannot answer costs the transcript its spelling hints, not the transcript.
func (r *Recorder) primingPrompt(ctx context.Context, since, until time.Time) string {
	episodes, err := r.store.EpisodesInWindow(ctx, since.Add(-contextMargin), until.Add(contextMargin), episodeLimit)
	if err != nil {
		slog.Warn("could not read desktop context to prime whisper", "error", err)
		return ""
	}
	return primingPrompt(episodes)
}
