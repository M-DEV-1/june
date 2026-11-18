// This file is the screen sweep: once a night, the dream brain reads what the screen showed after each of the user's open tasks was raised and says which of them that shows finished. The writing sweep (db.CloseDoneActionItems) closes a task only when minutes, a day's page or a note say so in matching words, and work done on screen — a PR merged in the browser, a ticket moved, a file sent — is almost never written up that way, so the morning brief went on chasing it.
package dream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"june/internal/memory"
	"june/internal/obs"
	"june/internal/util"
)

// screenDoneItemCap is how many open tasks one night checks, newest first. The prompt carries a few screen moments for each, so the cap is what bounds it.
const screenDoneItemCap = 15

// screenDoneHitsPerItem is how many screen moments are shown for one task: the ones the search ranks best.
const screenDoneHitsPerItem = 3

// screenDoneExcerpt is how much of one screen moment's text is shown, in runes.
const screenDoneExcerpt = 600

// screenDoneMinQuote is the shortest quote that can close a task. A one-word quote such as "Done" or "Merged" is found on every board and pull-request list and says nothing about which work it belongs to.
const screenDoneMinQuote = 12

// screenDonePrompt asks the dream brain which tasks the screen shows finished. The tasks follow, each with the screen moments captured after it was raised.
const screenDonePrompt = `These are the user's open tasks. Under each are moments from the user's screen captured after the task was raised: the app, the window title and the text that was on screen.

%s

For each task, decide whether these moments show that the work itself was finished: the pull request merged, the message or file sent, the ticket moved to done, the document submitted. A task that is only open, mentioned, planned, discussed or in progress on screen is not finished. Something else with a similar name being finished does not finish this task. When you are unsure, leave the task out.

For each finished task, quote the exact words from its own moments that show it, copied character for character.

Answer with JSON only: {"done":[{"id":task id,"quote":"exact words from that task's moments"}]}`

// screenDoneVerdict is the brain's answer: the tasks it says are finished, each with the words that show it.
type screenDoneVerdict struct {
	Done []struct {
		ID    int64  `json:"id"`
		Quote string `json:"quote"`
	} `json:"done"`
}

// screenDoneStage closes the open tasks the screen shows finished. Input: the night key. Output: how many tasks were closed, or a store or brain transport error. A task with no screen moment that names its work is not sent at all, and a night where no task has one makes no brain call.
// The brain's word is not enough on its own to close anything. Its quote has to be found in a moment that was shown for that same task, so a quote it invented, or took from another task's moments, closes nothing. A closed task records the quote as its evidence on its own line, and a task the user reopens is not checked again.
func (r *Runner) screenDoneStage(ctx context.Context, night string) (int, error) {
	ctx, span := obs.StartSpan(ctx, "Dream.ScreenDoneStage")
	defer span.End()
	open, err := r.store.OpenActionItems(ctx)
	if err != nil {
		return 0, err
	}
	slices.SortStableFunc(open, func(x, y memory.ActionItem) int { return y.Created.Compare(x.Created) })
	if len(open) > screenDoneItemCap {
		open = open[:screenDoneItemCap]
	}

	shown := map[int64][]string{}
	var b strings.Builder
	for _, a := range open {
		// An open task that still names the evidence that once closed it is one the user put back: reopening only changes the status. The moment that closed it is still on screen history, so checking it again would close it again every night.
		if a.DoneSource != "" {
			continue
		}
		words := memory.DistinctiveWords(a.Text)
		if len(words) == 0 {
			continue
		}
		since := a.Created
		if since.IsZero() {
			since = a.Raised
		}
		hits, err := r.store.SearchEpisodesSince(ctx, strings.Join(words, " "), since)
		if err != nil {
			return 0, err
		}
		if len(hits) > screenDoneHitsPerItem {
			hits = hits[:screenDoneHitsPerItem]
		}
		var moments []string
		for _, h := range hits {
			text := strings.TrimSpace(h.Content)
			if text == "" {
				continue
			}
			moments = append(moments, fmt.Sprintf("%s — %s, %s:\n%s", h.App, h.Title, h.CreatedAt.Local().Format("2 Jan 15:04"), util.Runes(text, screenDoneExcerpt)))
		}
		if len(moments) == 0 {
			continue
		}
		shown[a.NoteID] = moments
		fmt.Fprintf(&b, "Task %d: %s\n", a.NoteID, a.Text)
		for _, m := range moments {
			fmt.Fprintf(&b, "- %s\n", m)
		}
		b.WriteString("\n")
	}
	if len(shown) == 0 {
		return 0, nil
	}

	var v screenDoneVerdict
	if err := r.askJSON(ctx, night, "screen-done", fmt.Sprintf(screenDonePrompt, strings.TrimSpace(b.String())), &v); err != nil {
		if errors.Is(err, errUnparsable) {
			slog.Warn("dreaming: the screen sweep's answer did not parse, closing nothing tonight", "night", night)
			return 0, nil
		}
		return 0, err
	}
	closed := 0
	for _, d := range v.Done {
		moments, ok := shown[d.ID]
		quote := strings.TrimSpace(d.Quote)
		if !ok || len([]rune(quote)) < screenDoneMinQuote || !quotedFrom(quote, moments) {
			continue
		}
		delete(shown, d.ID)
		source := "your screen, " + dayOf(night).Format(time.DateOnly) + ": " + util.Runes(quote, 80)
		if err := r.store.SetActionDone(ctx, d.ID, source); err != nil {
			return closed, err
		}
		closed++
	}
	return closed, nil
}

// quotedFrom reports whether quote appears in one of the moments, ignoring case and runs of whitespace, which a model copying screen text does not keep exactly.
func quotedFrom(quote string, moments []string) bool {
	q := strings.Join(strings.Fields(strings.ToLower(quote)), " ")
	for _, m := range moments {
		if strings.Contains(strings.Join(strings.Fields(strings.ToLower(m)), " "), q) {
			return true
		}
	}
	return false
}
