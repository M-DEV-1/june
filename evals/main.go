package main

// The eval runner. `go run ./evals` runs all three tracks against the live machine's real data and writes a timestamped scorecard into evals/runs/, one file per run, keyed by the commit it measured. That directory is the quality history: a run from a later commit sits next to a run from an earlier one and the difference is what changed.
//
// Nothing here writes to live data. The sqlite store is snapshotted with VACUUM INTO before it is opened, because db.New runs its schema migrations on open and the live daemon is using that file. ora.log and the recordings directory are read and never written. The vector index is the daemon's own, reached over /vector/search, which only reads.

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/joho/godotenv"
	_ "modernc.org/sqlite"

	"ora/internal/config"
	"ora/internal/db"
)

func main() {
	tracks := flag.String("tracks", "1,2,3", "which tracks to run, comma separated: 1 memory replay, 2 conversation judge, 3 minutes judge")
	turnCap := flag.Int("turns", 40, "track 2: score at most this many of the most recent turn pairs")
	outDir := flag.String("out", "evals/runs", "directory the scorecard is written to")
	questionsPath := flag.String("questions", "evals/questions.jsonl", "track 1: the question set")
	flag.Parse()

	// The judge is chatty on stderr through slog if internal packages log; keep it to warnings so the run's own output stays readable.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))

	if err := run(*tracks, *turnCap, *outDir, *questionsPath); err != nil {
		fmt.Fprintf(os.Stderr, "eval run failed: %v\n", err)
		os.Exit(1)
	}
}

// scorecard is everything one run measured, held until the markdown is written so a track that fails late does not lose the tracks that already passed.
type scorecard struct {
	Started time.Time
	SHA     string
	Notes   []string
	T1      []track1Result
	T2      []track2Result
	T3      []track3Result
	Ran     map[string]bool
}

func run(tracks string, turnCap int, outDir, questionsPath string) error {
	// The API key lives in the repo's .env, the same file cmd/root.go loads at startup.
	_ = godotenv.Load()
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("GEMINI_API_KEY is not set (expected in .env)")
	}

	ctx := context.Background()
	sel := map[string]bool{}
	for _, t := range strings.Split(tracks, ",") {
		sel[strings.TrimSpace(t)] = true
	}
	card := scorecard{Started: time.Now(), SHA: gitSHA(), Ran: sel}

	j, err := newJudge(ctx, apiKey)
	if err != nil {
		return err
	}

	dataDir := config.DataDir()

	if sel["1"] {
		fmt.Println("track 1 — memory replay")
		qs, err := loadQuestions(questionsPath)
		if err != nil {
			return fmt.Errorf("track 1: %w", err)
		}
		snapshot, err := snapshotDB(filepath.Join(dataDir, "db"))
		if err != nil {
			return fmt.Errorf("track 1: %w", err)
		}
		defer os.RemoveAll(filepath.Dir(snapshot))

		store, err := db.New(snapshot)
		if err != nil {
			return fmt.Errorf("track 1: open snapshot: %w", err)
		}
		embedder, index := newDaemonClients()
		store.SetEmbedder(embedder)
		store.SetVectorIndex(index)
		store.SetVectorSimilarityFloor(float32(config.LoadConfig().Embed.Floor()))

		// One probe before the run: a dead daemon turns every question into a silently lexical-only search, which would read as a retrieval regression on the scorecard rather than as the outage it is.
		if _, err := embedder.Embed(ctx, "RETRIEVAL_QUERY", "probe"); err != nil {
			card.Notes = append(card.Notes, fmt.Sprintf("daemon /embed unreachable (%v) — track 1 ran lexical-only, treat its numbers as a floor", err))
			fmt.Println("  WARNING: /embed unreachable, hybrid search will degrade to lexical-only")
		}
		card.T1 = runTrack1(ctx, store, j, qs)
		store.Close()
	}

	if sel["2"] {
		fmt.Println("track 2 — conversation judge")
		pairs, err := loadTurnPairs(filepath.Join(dataDir, "ora.log"), turnCap)
		if err != nil {
			return fmt.Errorf("track 2: %w", err)
		}
		fmt.Printf("  %d turn pairs since \"ora said\" logging began\n", len(pairs))
		card.T2 = runTrack2(ctx, j, pairs)
	}

	if sel["3"] {
		fmt.Println("track 3 — minutes judge")
		dirs, err := findMinutes(filepath.Join(dataDir, "recordings"))
		if err != nil {
			return fmt.Errorf("track 3: %w", err)
		}
		card.T3 = runTrack3(ctx, j, dirs)
	}

	path, err := writeScorecard(card, outDir)
	if err != nil {
		return err
	}
	fmt.Printf("\nscorecard: %s\n", path)
	fmt.Print(summary(card))
	return nil
}

// snapshotDB copies the live sqlite database into a fresh temp directory with VACUUM INTO, sqlite's own consistent-copy statement, and returns the copy's path. The source is opened read-only so a runner bug can never touch live data; the caller removes the returned file's directory when done. Input: the live database path. Output: the snapshot path.
func snapshotDB(livePath string) (string, error) {
	dir, err := os.MkdirTemp("", "ora-eval-db-")
	if err != nil {
		return "", err
	}
	dest := filepath.Join(dir, "db")

	src, err := sql.Open("sqlite", "file:"+livePath+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return "", fmt.Errorf("open live db read-only: %w", err)
	}
	defer src.Close()
	if _, err := src.Exec("VACUUM INTO ?", dest); err != nil {
		return "", fmt.Errorf("snapshot live db: %w", err)
	}
	return dest, nil
}

// gitSHA returns the short commit the run measured, or "nogit" when the tree is not a repository. The scorecard is named after it so a run can be traced back to the code it scored.
func gitSHA() string {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "nogit"
	}
	return strings.TrimSpace(string(out))
}
