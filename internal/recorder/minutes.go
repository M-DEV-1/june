package recorder

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"ora/internal/config"

	"google.golang.org/genai"
)

// minutesInstruction tells the model what to make of the transcript. The microphone side is [me]; the system-audio side is [call], a single pooled label covering every remote voice, and the model's job is to put names to it from the screen context and from what was said.
const minutesInstruction = `You are writing the minutes of a meeting from an automatic transcript.

The transcript has two speaker labels and only two:
  [me]   — the person whose computer recorded this, captured from their microphone.
  [call] — everyone else on the call, captured from the computer's speakers. Several different people share this one label; the recording cannot tell their voices apart.

Your job includes working out WHO said what on the [call] side. Two sources let you do it:
  1. The screen context below. It is what the meeting app itself showed on screen while the call ran — participant tiles, "X is presenting", chat messages with sender names, the meeting title. Names there are real and correctly spelled.
  2. The transcript itself. People introduce themselves, address each other by name, and refer to their own work.

Rules for naming:
  - Use a person's real name whenever the context or the words support it.
  - When you cannot get a name, describe the role instead: "the interviewer", "the person presenting", "a second participant".
  - Never write "them", "the other side", or "the other participant" as if it were one person.
  - Do not invent people, and do not assign a name to a line unless something actually points to it. Prefer a role description over a guess.
  - The transcript comes from speech recognition, so names in it may be misspelled. Where the screen context has the same name spelled properly, use that spelling.
  - Name the [me] speaker too if the screen context shows whose machine this is. Otherwise call them "the person recording". Never write "[me]" in the minutes.

Write markdown with these sections, in this order:

# Meeting minutes
## Attendees
Everyone who took part, from both the screen context and the transcript. Name each person, and say what tells you they were there (on screen, spoke, named by someone else). Say "unclear" for a voice you could not place.
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

	if timeline := r.desktopTimeline(ctx, startedAt, stoppedAt); timeline != "" {
		b.WriteString("\nScreen context — what was on the user's screens while the meeting ran, in order, as read out of the windows themselves. This is where participant names, presenter labels and chat senders come from. It is context, not speech: never quote it as something someone said.\n")
		b.WriteString(timeline)
	}

	b.WriteString("\nTranscript:\n")
	b.WriteString(transcript)
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

// geminiMinutes is the default minutes seam: one non-streaming GenerateContent call, the same shape memory.GeminiSummarizer uses for its background summaries.
func (r *Recorder) geminiMinutes(ctx context.Context, prompt string) (string, error) {
	if r.apiKey == "" {
		return "", fmt.Errorf("no GEMINI_API_KEY, cannot summarise the meeting (the transcript is still on disk)")
	}
	client, err := genai.NewClient(ctx, &genai.ClientConfig{APIKey: r.apiKey, Backend: genai.BackendGeminiAPI})
	if err != nil {
		return "", fmt.Errorf("gemini client: %w", err)
	}
	resp, err := client.Models.GenerateContent(ctx, config.TextModel, genai.Text(prompt), nil)
	if err != nil {
		return "", fmt.Errorf("generate minutes: %w", err)
	}
	text := resp.Text()
	if text == "" {
		return "", fmt.Errorf("gemini returned no minutes text")
	}
	return text, nil
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
