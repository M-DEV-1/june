package recorder

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/joho/godotenv"

	"ora/internal/config"
	"ora/internal/db"
)

// The two lists of recording directory names this hand-run tool works on, empty by default so a plain "go test" does nothing to the real data directory.
var (
	liveBackfill   = flag.String("live.backfill", "", "comma-separated recording directory names whose existing minutes should be read into personal context")
	liveRegenerate = flag.String("live.regenerate", "", "comma-separated recording directory names whose minutes should be archived and written again from the transcript")
)

// TestLive_BackfillAndRegenerate is the hand-run tool for the real recordings under the real data directory. It has two jobs, in this order: read the minutes already written for the meetings named by -live.backfill and let the personal context updater learn the people in them, then archive and rewrite the minutes for the meetings named by -live.regenerate, which now get written with those people in the prompt.
// It writes to the live database and the live recording directories, and it costs Gemini calls, so it is skipped unless asked for by name:
//
//	go test ./internal/recorder/ -run Live -v -timeout 20m -live.backfill=2026-08-28T14-03-50 -live.regenerate=2026-08-28T14-44-12
//
// Whisper never runs: regeneration goes through the sweep's transcript-with-no-minutes case, which summarises the transcript already on disk. Minutes are never filed as a note either, since the originals already were and a second copy would only be read back twice.
func TestLive_BackfillAndRegenerate(t *testing.T) {
	backfill, regenerate := *liveBackfill, *liveRegenerate
	if backfill == "" && regenerate == "" {
		t.Skip("pass -live.backfill and/or -live.regenerate with recording directory names to run against the real data directory")
	}
	_ = godotenv.Load("../../.env")
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		t.Fatal("GEMINI_API_KEY is not set, so there is nothing to summarise with")
	}

	store, err := db.New(filepath.Join(config.DataDir(), "db"))
	if err != nil {
		t.Fatalf("open the live database: %v", err)
	}
	defer store.Close()

	dataDir := config.DataDir()
	r := &Recorder{dataDir: dataDir, store: readOnlyNotes{store}, apiKey: key}
	r.minutes = r.geminiMinutes
	r.notify = func(title, body string) { t.Logf("notify: %s — %s", title, body) }
	// The machine may be on battery and the audio is beside the transcripts either way, so a path that would start whisper is a bug in this run, not something to fall back from.
	r.findWhisper = func(string) (string, error) { return "", errors.New("this run must never transcribe") }
	r.whisper = func(context.Context, string, string, string, string, time.Duration) ([]Segment, error) {
		return nil, errors.New("this run must never transcribe")
	}
	ctx := context.Background()

	for _, name := range split(backfill) {
		dir := filepath.Join(dataDir, "recordings", name)
		minutes, err := os.ReadFile(filepath.Join(dir, "minutes.md"))
		if err != nil {
			t.Fatalf("read the minutes to backfill from: %v", err)
		}
		s := pickupSession(dir)
		t.Logf("backfilling personal context from %s (%s to %s)", name, s.startedAt.Format(time.RFC3339), s.stoppedAt.Format(time.RFC3339))
		r.updatePersonalContext(ctx, string(minutes), s.startedAt, s.stoppedAt)
	}

	entries, err := store.PersonalContext(ctx)
	if err != nil {
		t.Fatalf("read personal context back: %v", err)
	}
	for _, e := range entries {
		t.Logf("personal context now holds %q: %s", e.Subject, e.Content)
	}

	for _, name := range split(regenerate) {
		dir := filepath.Join(dataDir, "recordings", name)
		if err := os.Rename(filepath.Join(dir, "minutes.md"), filepath.Join(dir, nextVersion(t, dir))); err != nil {
			t.Fatalf("archive the previous minutes: %v", err)
		}
		s, ok := unfinished(dir)
		if !ok || !s.fromTranscript {
			t.Fatalf("%s should now look to the sweep like a transcript with no minutes", name)
		}
		if err := r.process(ctx, s); err != nil {
			t.Fatalf("regenerate the minutes for %s: %v", name, err)
		}
		out, err := os.ReadFile(filepath.Join(dir, "minutes.md"))
		if err != nil {
			t.Fatalf("read the regenerated minutes: %v", err)
		}
		t.Logf("regenerated minutes for %s:\n%s", name, out)
	}
}

// readOnlyNotes is the live store with the note-filing dropped. Regenerating minutes for a meeting that was already summarised must not file a second copy of them for the memory to read back.
type readOnlyNotes struct{ *db.Store }

func (readOnlyNotes) LogNote(ctx context.Context, content, kind string) (int64, error) { return 0, nil }

// nextVersion is the name to archive a directory's current minutes.md under: minutes.v1.md, or the next number up when earlier versions are already kept there.
func nextVersion(t *testing.T, dir string) string {
	t.Helper()
	for n := 1; ; n++ {
		name := "minutes.v" + strconv.Itoa(n) + ".md"
		if !exists(filepath.Join(dir, name)) {
			return name
		}
	}
}

// split reads a comma-separated list of recording directory names, ignoring blanks.
func split(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
