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
const minutesInstruction = `You are writing the minutes of a meeting from an automatic transcript.

The transcript has two speaker labels and only two:
  [me]   — the person whose computer recorded this, captured from their microphone.
  [call] — everyone else on the call, captured from the computer's speakers. Several different people share this one label; the recording cannot tell their voices apart.

Your job includes working out WHO said what on the [call] side. Only these things name a person:
  1. The meeting app's own display of who is in the call: a participant tile, "X is presenting", a chat message with a sender name, the meeting title. Names there are real and correctly spelled.
  2. The transcript itself: someone introduces themselves, or one person addresses another by name.

These do NOT name anybody, and you must never take a name from them:
  - A username, commit author, pull request author, repository owner, file header, profile page, or issue assignee shown on screen. People read other people's code and pages all day; a name on a repository page is not a person in the room and is not whose computer this is.
  - A name written in a document, spreadsheet, ticket, email or web page that happened to be open.
  - A name that appears only once, in passing, with nothing tying it to a voice.

Rules for naming:
  - Attribute a line to a named person only when one of the two sources above actually points to it. Otherwise describe the role: "the interviewer", "the person presenting", "a second participant".
  - Never write "them", "the other side", or "the other participant" as if it were one person.
  - Several people share the [call] label and the recording cannot tell their voices apart, so a name is a claim you must be able to point at evidence for. When two people on the [call] side are both plausible for a line, say the speaker is unclear rather than picking one.
  - The transcript comes from speech recognition, so names in it may be misspelled. Where the screen context has the same name spelled properly, use that spelling.
  - For the [me] speaker: this is always the same one person, the owner of this computer. What the user has told Ora about themselves is given below under "About the person recording" — if it names them, that is who [me] is, and it outranks anything on screen. Failing that, use a name they are addressed by in the call. Nothing else identifies whose machine this is — not the code on screen, not the accounts signed in. Otherwise call them "the person recording". Never write "[me]" in the minutes.

Write markdown with these sections, in this order:

# Meeting minutes
## Attendees
Two lists, and every name goes in exactly one of them.
**In the meeting** — only people with evidence they were in the call: they spoke, they were spoken TO by name during it, or the meeting app or a shared working surface showed them taking part (participant tile, "presenting" label, a message they sent in the meeting's chat or shared whiteboard while it ran). Say which of those it was. Being talked ABOUT is not presence: an instruction, plan, or errand involving someone who never speaks and is never spoken to puts them under Mentioned, however often their name comes up. The recording person is one entry here once you know their name, written as "Their Name (recording)" — never also a separate "the person recording" bullet.
**Mentioned or on screen only** — names that came up in talk or on screen with nothing showing they were in the call: a commit author, a ticket assignee, an account name, someone discussed. Drop this list if there are none.
A name you can only hedge about ("referenced via screen context") goes in the second list, never the first. Say "unclear" for a voice you could not place at all.
## Key points
What was discussed. Bullet points, plain language, attributing points to named people where you can.
## Decisions
What was actually decided, and who decided it. If nothing was decided, say so.
## Action items
Who agreed to do what, one bullet each, with the owner named. If no owner was named, write "owner unclear".

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

// screenTextBudget caps how much of one episode's captured window text goes into the prompt. The names and presenter labels a meeting app shows sit near the front of what accessibility reads out, so the head of the text is the part worth keeping.
const screenTextBudget = 400

// desktopTimeline renders the meeting window's episodes as "HH:MM app — window title" lines followed by the text read off that window, which is what carries participant names ("Vikram Goel (Presenting)", chat senders, the meeting title).
// Consecutive episodes with the same app, title and text collapse to one line, since the tracker samples every couple of seconds and most samples repeat.
func (r *Recorder) desktopTimeline(ctx context.Context, since, until time.Time) string {
	episodes, err := r.store.EpisodesInWindow(ctx, since, until, episodeLimit)
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
		text = truncate(strings.Join(strings.Fields(text), " "), screenTextBudget)

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
	episodes, err := r.store.EpisodesInWindow(ctx, since, until, episodeLimit)
	if err != nil {
		slog.Warn("could not read desktop context to prime whisper", "error", err)
		return ""
	}
	return primingPrompt(episodes)
}
