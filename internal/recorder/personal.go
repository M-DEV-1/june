package recorder

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"ora/internal/db"
)

// personalUpdateInstruction is the bar the meeting updater has to clear before it writes anything into personal context. The store is small, permanent and read into every conversation, so the default answer is to write nothing.
const personalUpdateInstruction = `You maintain a small store of things known for certain about one person — the user whose computer recorded this meeting. You have just been given the minutes of that meeting, what was on their screens while it ran, and everything the store holds right now.

Decide whether the meeting changed anything in the store. Almost always the answer is nothing. What you write is permanent and is read back into every conversation the user has, so the bar is high:

  - If the store already says it, write nothing for it. Only a real change earns an entry: a person who is not there yet, a fact about a person that has actually moved on, a preference or a life fact the user stated.
  - Only durable things about people, relationships, preferences, and the user's life and work. What was decided in this meeting, what is being built this week, what someone is working on right now — none of that belongs here. That is kept elsewhere.
  - A person earns an entry only with the same evidence the minutes needed: they spoke in the call, they were addressed by name during it, or the meeting app showed them in it. A username, commit author, ticket assignee, page owner or account name on screen is NEVER a person here.
  - Do not write an entry about the user themself. The "identity" subject already covers who they are.
  - When you update a person who is already stored, rewrite their whole entry: keep what still holds, fold in what is new, and keep it to a few sentences. Include when the user last worked with them, by date, so the entry says how current it is.
  - Subjects are short and lowercase, hyphenated: a person's own name ("priya-shah"), or an area of preference ("preferences-communication").

Reply with JSON and nothing else: {"updates":[{"subject":"...","content":"..."}]}. Write {"updates":[]} when the meeting changed nothing, which is the usual answer.`

// personalUpdate is one subject the model wants written, exactly as SetPersonalContext takes it.
type personalUpdate struct {
	Subject string `json:"subject"`
	Content string `json:"content"`
}

// personalUpdatePrompt assembles the updater's input: the bar, the store as it stands, the minutes, and the screen timeline the minutes were written from.
// Input: the current entries, the finished minutes, the rendered desktop timeline. Output: the whole prompt string.
func personalUpdatePrompt(entries []db.PersonalEntry, minutes, timeline string) string {
	var b strings.Builder
	b.WriteString(personalUpdateInstruction)

	b.WriteString("\n\nThe store as it stands:\n")
	if len(entries) == 0 {
		b.WriteString("  (empty)\n")
	}
	for _, e := range entries {
		fmt.Fprintf(&b, "  %s: %s\n", e.Subject, strings.TrimSpace(e.Content))
	}

	b.WriteString("\nThe minutes of the meeting that just finished:\n")
	b.WriteString(minutes)
	if timeline != "" {
		b.WriteString("\n\nScreen context — what was on their screens while the meeting ran. Names here are not proof anybody was in the call.\n")
		b.WriteString(timeline)
	}
	return b.String()
}

// updatePersonalContext asks the model whether the meeting that just finished changed anything in personal context, and writes whatever comes back. Best-effort throughout: nothing here can fail a finished recording, so every error is logged and dropped.
// Every write is logged with its subject, since this is the one path that puts something in the store without the user saying it in so many words.
func (r *Recorder) updatePersonalContext(ctx context.Context, minutes string, startedAt, stoppedAt time.Time) {
	entries, err := r.store.PersonalContext(ctx)
	if err != nil {
		slog.Warn("could not read personal context to update it after a meeting", "error", err)
		return
	}

	out, err := r.minutes(ctx, personalUpdatePrompt(entries, minutes, r.desktopTimeline(ctx, startedAt, stoppedAt)))
	if err != nil {
		slog.Warn("could not check whether the meeting changed personal context", "error", err)
		return
	}

	for _, u := range parsePersonalUpdates(out) {
		if strings.TrimSpace(u.Subject) == "" || strings.TrimSpace(u.Content) == "" {
			continue
		}
		if err := r.store.SetPersonalContext(ctx, u.Subject, u.Content); err != nil {
			slog.Warn("could not write a personal context entry from a meeting", "subject", u.Subject, "error", err)
			continue
		}
		slog.Info("personal context updated", "subject", u.Subject, "content", u.Content)
	}
}

// parsePersonalUpdates reads the updates out of the model's reply. The reply is meant to be bare JSON but often arrives fenced or with a sentence around it, so the outermost braces are taken and anything that still doesn't parse means no updates — writing nothing is always the safe reading.
func parsePersonalUpdates(reply string) []personalUpdate {
	start, end := strings.Index(reply, "{"), strings.LastIndex(reply, "}")
	if start < 0 || end < start {
		return nil
	}
	var out struct {
		Updates []personalUpdate `json:"updates"`
	}
	if err := json.Unmarshal([]byte(reply[start:end+1]), &out); err != nil {
		slog.Warn("could not read the personal context updates from the model's reply", "error", err, "reply", reply)
		return nil
	}
	return out.Updates
}
