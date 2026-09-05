// Command backfill-actions is a one-off: it lifts the action items out of the meeting minutes already on file, so work agreed before action items were tracked is not lost when its minutes age out of the brief's window. The minutes themselves are never touched. Run with -write to commit; without it, it only prints what it would create.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"ora/internal/config"
	"ora/internal/db"
	"ora/internal/memory"
)

func main() {
	write := flag.Bool("write", false, "actually file the action items")
	del := flag.String("delete", "", "delete these note ids (comma separated), removing their FTS rows and vectors with them")
	set := flag.String("set", "", "apply corrections instead of backfilling: id=status/priority, comma separated (either side may be blank, e.g. 139=done/ or 137=/low)")
	flag.Parse()

	store, err := db.New(filepath.Join(config.DataDir(), "db"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "open store:", err)
		os.Exit(1)
	}
	defer store.Close()

	ctx := context.Background()
	if *del != "" {
		for _, idStr := range strings.Split(*del, ",") {
			id, err := strconv.ParseInt(strings.TrimSpace(idStr), 10, 64)
			if err != nil {
				fmt.Fprintln(os.Stderr, "bad id:", idStr)
				continue
			}
			if err := store.DeleteNote(ctx, id); err != nil {
				fmt.Fprintln(os.Stderr, "delete:", err)
				continue
			}
			fmt.Printf("deleted #%d\n", id)
		}
		return
	}
	if *set != "" {
		applyCorrections(ctx, store, *set)
		return
	}

	notes, err := store.GetNotes(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read notes:", err)
		os.Exit(1)
	}

	total := 0
	for _, n := range notes {
		if n.Kind != "meeting" {
			continue
		}
		items := memory.ParseMinutesActions(n.Content, memory.MinutesLabel(n.Content), n.CreatedAt)
		kept := filterMine(items)
		if len(kept) == 0 && len(items) > 0 {
			fmt.Printf("\nnote #%d — %q: %d items, none the user's — meeting skipped\n", n.ID, memory.MinutesLabel(n.Content), len(items))
		}
		items = kept
		if len(items) == 0 {
			continue
		}
		fmt.Printf("\nnote #%d — %q (%s)\n", n.ID, memory.MinutesLabel(n.Content), n.CreatedAt.Format("2006-01-02"))
		for _, a := range items {
			fmt.Printf("  %s\n", a.Note())
		}
		total += len(items)
		if *write {
			added, err := store.AddActionItems(ctx, items)
			if err != nil {
				fmt.Fprintln(os.Stderr, "  file:", err)
				continue
			}
			fmt.Printf("  -> filed %d new (%d already on file)\n", added, len(items)-added)
		}
	}
	fmt.Printf("\n%d action items found across the minutes on file.\n", total)
	if !*write {
		fmt.Println("Dry run. Re-run with -write to file them.")
	}
}

// filterMine keeps only the action items that might be the user's own, the same per-item rule internal/recorder/recorder.go's liftActionItems applies to a meeting's minutes as it is written, so a backfill rerun files the same rows the daemon would have filed on the day of the meeting. Input: every action item parsed out of one meeting's minutes. Output: the ones owed by "Me" or by nobody named at all.
func filterMine(items []memory.ActionItem) []memory.ActionItem {
	var out []memory.ActionItem
	for _, a := range items {
		if a.Mine() {
			out = append(out, a)
		}
	}
	return out
}

// applyCorrections sets the status and priority of the named action items, which is what the user's own reading of the morning brief amounts to before the conversational tool exists to say it out loud.
func applyCorrections(ctx context.Context, store *db.Store, spec string) {
	for _, part := range strings.Split(spec, ",") {
		idStr, rest, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			fmt.Fprintln(os.Stderr, "bad correction:", part)
			continue
		}
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			fmt.Fprintln(os.Stderr, "bad id:", idStr)
			continue
		}
		status, priority, _ := strings.Cut(rest, "/")
		if status != "" {
			if err := store.SetActionStatus(ctx, id, status); err != nil {
				fmt.Fprintln(os.Stderr, "  status:", err)
				continue
			}
		}
		if priority != "" {
			if err := store.SetActionPriority(ctx, id, priority); err != nil {
				fmt.Fprintln(os.Stderr, "  priority:", err)
				continue
			}
		}
		fmt.Printf("#%d -> %s\n", id, rest)
	}
}
