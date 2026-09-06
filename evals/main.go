package main

// The eval runner. `go run ./evals` runs all three tracks against the live machine's real data and writes a timestamped scorecard into evals/runs/, one file per run, keyed by the commit it measured. That directory is the quality history: a run from a later commit sits next to a run from an earlier one and the difference is what changed.
//
// Tracks 1 to 9 write no live data. The sqlite store is snapshotted with VACUUM INTO before it is opened, because db.New runs its schema migrations on open and the live daemon is using that file. ora.log and the recordings directory are read and never written. The vector index is the daemon's own, reached over /vector/search, which only reads.
//
// Tracks 10 and 11 are the exception and do the opposite: they drive the running daemon over HTTP, so every tool the model reaches for runs for real — clicks and keystrokes land on whatever is on screen, and act runs, episodes and any note the model saves stay in the live store (track 10 deletes only the conversation it opened; track 11 deletes nothing). That is why liveGuard refuses to run either against the default port unless -live says so.

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

	"ora/internal/agent"
	"ora/internal/brain"
	"ora/internal/config"
	"ora/internal/db"
	"ora/internal/study"
)

func main() {
	tracks := flag.String("tracks", "1,2,3", "which tracks to run, comma separated: 1 memory replay, 2 conversation judge, 3 minutes judge, 5 counterfactual replay, 6 distillation study, 7 trajectory eval, 9 gold set, 10 computer-use eval, 11 long-task job eval")
	session := flag.String("session", "", "track 5: replay only sessions whose start time begins with this prefix (e.g. 2026-08-30); empty means the most recent 3")
	turnCap := flag.Int("turns", 40, "track 2: score at most this many of the most recent turn pairs")
	trajTurns := flag.Int("traj-turns", 10, "track 7: how many user messages the roleplay user sends in each arm's conversation")
	trajModel := flag.String("traj-model", config.TextModel, "track 7: the model the gemini arm runs on — the live session's own native-audio model cannot do text function calling, so this defaults to the text model")
	outDir := flag.String("out", "evals/runs", "directory the scorecard is written to")
	questionsPath := flag.String("questions", "evals/questions.jsonl", "track 1: the question set")
	toolPath := flag.Bool("tool-path", false, "track 1: replay questions through the agent's real query_memory tool (ExecuteTool, honoring each question's args) instead of calling HybridSearch directly")
	flag.Parse()

	// The judge is chatty on stderr through slog if internal packages log; keep it to warnings so the run's own output stays readable.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))

	if err := run(*tracks, *turnCap, *outDir, *questionsPath, *toolPath, *session, *trajTurns, *trajModel); err != nil {
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
	T8      []track8Result
	T8Arms  []arm
	Ran     map[string]bool
}

func run(tracks string, turnCap int, outDir, questionsPath string, toolPath bool, session string, trajTurns int, trajModel string) error {
	// The API key lives in the repo's .env, the same file cmd/root.go loads at startup.
	_ = godotenv.Load()

	ctx := context.Background()
	sel := map[string]bool{}
	for _, t := range strings.Split(tracks, ",") {
		sel[strings.TrimSpace(t)] = true
	}
	card := scorecard{Started: time.Now(), SHA: gitSHA(), Ran: sel}

	// Every track but 10 either calls Gemini itself or scores its material with the Gemini judge, so the key and the judge are built only when one of those is selected: track 10 alone talks to nothing but the running daemon.
	apiKey := os.Getenv("GEMINI_API_KEY")
	var j *judge
	if needsGeminiKey(sel) {
		if apiKey == "" {
			return fmt.Errorf("GEMINI_API_KEY is not set (expected in .env)")
		}
		var err error
		j, err = newJudge(ctx, apiKey)
		if err != nil {
			return err
		}
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
		search := directSearch(store)
		if toolPath {
			// The agent handle exists only to reach ExecuteTool; no mic, speaker, compiler, or live session is involved, and the key is never used by query_memory.
			search = toolPathSearch(agent.NewAgent(nil, nil, store, nil, apiKey))
			card.Notes = append(card.Notes, "track 1 replayed through the agent's query_memory tool (ExecuteTool), honoring per-question args")
		}
		card.T1 = runTrack1(ctx, search, j, qs)
		store.Close()
	}

	if sel["8"] {
		fmt.Println("track 8 — context vs capacity")
		qs, err := loadQuestions(questionsPath)
		if err != nil {
			return fmt.Errorf("track 8: %w", err)
		}
		snapshot, err := snapshotDB(filepath.Join(dataDir, "db"))
		if err != nil {
			return fmt.Errorf("track 8: %w", err)
		}
		defer os.RemoveAll(filepath.Dir(snapshot))

		store, err := db.New(snapshot)
		if err != nil {
			return fmt.Errorf("track 8: open snapshot: %w", err)
		}
		embedder, index := newDaemonClients()
		store.SetEmbedder(embedder)
		store.SetVectorIndex(index)
		store.SetVectorSimilarityFloor(float32(config.LoadConfig().Embed.Floor()))
		// The same probe track 1 runs: a dead daemon makes every question lexical-only, which would read as a retrieval failure on the cross-tab rather than as the outage it is — and on this track that misreading lands in the "insufficient rows" column and blames retrieval for a machine that was simply not answering.
		if _, err := embedder.Embed(ctx, "RETRIEVAL_QUERY", "probe"); err != nil {
			card.Notes = append(card.Notes, fmt.Sprintf("daemon /embed unreachable (%v) — track 8 ran lexical-only, treat its retrieval column as a floor", err))
			fmt.Println("  WARNING: /embed unreachable, hybrid search will degrade to lexical-only")
		}
		search := directSearch(store)
		if toolPath {
			search = toolPathSearch(agent.NewAgent(nil, nil, store, nil, apiKey))
			card.Notes = append(card.Notes, "track 8 retrieved through the agent's query_memory tool (ExecuteTool), honoring per-question args")
		}
		arms := capacityArms()
		// The clock is to the minute, not the day: two runs of the same commit on one evening is the normal case when a fix is being measured, and a day-and-sha name silently overwrote the "before" half of exactly that comparison.
		frozen := filepath.Join(outDir, fmt.Sprintf("%s-%s-track8.json", card.Started.Format("2006-01-02-1504"), gitSHA()))
		card.T8 = runTrack8(ctx, search, j, arms, qs, frozen)
		card.T8Arms = arms
		card.Notes = append(card.Notes, fmt.Sprintf("track 8 rows and answers frozen to %s", frozen))
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

	if sel["5"] {
		fmt.Println("track 5 — counterfactual replay")
		note, err := runTrack5(ctx, j, teacherBrain(), filepath.Join(dataDir, "ora.log"), "evals/replays", session)
		if err != nil {
			return fmt.Errorf("track 5: %w", err)
		}
		card.Notes = append(card.Notes, note)
		fmt.Println("  " + note)
	}

	if sel["6"] {
		fmt.Println("track 6 — distillation study")
		replays, err := filepath.Glob("evals/replays/*.md")
		if err != nil {
			return fmt.Errorf("track 6: %w", err)
		}
		traces, err := filepath.Glob(filepath.Join(dataDir, "dreams", "*.jsonl"))
		if err != nil {
			return fmt.Errorf("track 6: %w", err)
		}
		res, err := study.Study(ctx, teacherBrain(), replays, traces, filepath.Join(dataDir, "study"))
		if err != nil {
			return fmt.Errorf("track 6: %w", err)
		}
		note := fmt.Sprintf("study: %d replays, %d traces (%d trace lines skipped) — %d lessons added, report at %s",
			res.ReplaysRead, res.TracesRead, res.LinesSkipped, res.LessonsAdded, res.ReportPath)
		card.Notes = append(card.Notes, note)
		fmt.Println("  " + note)
	}

	if sel["7"] {
		fmt.Println("track 7 — trajectory eval")
		note, err := runTrack7(ctx, j, apiKey, dataDir, trajModel, trajTurns, "evals/trajectories")
		if err != nil {
			return fmt.Errorf("track 7: %w", err)
		}
		card.Notes = append(card.Notes, note)
		fmt.Println("  " + note)
	}

	if sel["9"] {
		fmt.Println("track 9 — gold set")
		// The gold track's own flags live beside it in track9_gold.go, so adding it cost this switch six lines rather than four more parameters on run().
		note, err := runTrack9(ctx, dataDir, apiKey, *goldPath, *goldArmNames, outDir)
		if err != nil {
			return fmt.Errorf("track 9: %w", err)
		}
		card.Notes = append(card.Notes, note)
		fmt.Println("  " + note)
	}

	if sel["10"] {
		fmt.Println("track 10 — computer-use eval")
		// The screen-task table and its own --brain flag live beside it in track10_act.go.
		note := liveGuard(daemonAddr, *live)
		if note != "" {
			note = "track 10 " + note
		} else {
			var err error
			note, err = runTrack10(ctx, daemonAddr)
			if err != nil {
				return fmt.Errorf("track 10: %w", err)
			}
		}
		card.Notes = append(card.Notes, note)
		fmt.Println("  " + note)
	}

	if sel["11"] {
		fmt.Println("track 11 — long-task job eval")
		// The job task table and its own -act11-tasks selector live beside it in track11_actjob.go; it shares track 10's -brain flag and daemon plumbing.
		note := liveGuard(daemonAddr, *live)
		if note != "" {
			note = "track 11 " + note
		} else {
			var err error
			note, err = runTrack11(ctx, daemonAddr)
			if err != nil {
				return fmt.Errorf("track 11: %w", err)
			}
		}
		card.Notes = append(card.Notes, note)
		fmt.Println("  " + note)
	}

	path, err := writeScorecard(card, outDir)
	if err != nil {
		return err
	}
	fmt.Printf("\nscorecard: %s\n", path)
	fmt.Print(summary(card))
	return nil
}

// live is the flag that lets tracks 10 and 11 run against the daemon on the default port. It is off by default because both drive whatever is on the user's screen and write to the user's own store.
var live = flag.Bool("live", false, "tracks 10 and 11: let them drive the daemon on the default port, which clicks and types on the real screen and writes to the real store")

// liveGuard is the refusal that keeps tracks 10 and 11 off the user's own machine unless the run asked for it. Input: the daemon base URL the track would drive, and whether -live was passed. Output: the one-line reason to print and record on the scorecard, or "" when the run may go ahead — which it may whenever -live was passed, or whenever the track is pointed at a daemon other than the one on the default port.
func liveGuard(baseURL string, live bool) string {
	if live || baseURL != daemonAddr {
		return ""
	}
	return fmt.Sprintf("skipped: %s is the live daemon, and these tasks click and type on whatever is on screen and leave their act runs, episodes and notes in the real store. Pass -live to run them there anyway, or start a daemon on its own ORA_PORT and ORA_DATA_DIR over a snapshot and point the track at that.", daemonAddr)
}

// needsGeminiKey reports whether the selected tracks need a Gemini API key and the Gemini judge built from it. Input: the set of track names the run selected. Output: false only when tracks 10 and 11 are the only ones selected — both drive the running daemon over HTTP and score each task against a fixed rule, so neither calls Gemini directly and both must still run on a day the Gemini quota is spent.
func needsGeminiKey(sel map[string]bool) bool {
	for track := range sel {
		if track != "10" && track != "11" {
			return true
		}
	}
	return false
}

// teacherBrain is the Claude teacher tracks 5 and 6 both study against: the machine's own login, sonnet unless the config's brain block pins a claude-cli model. Sonnet is the deliberate default: both tracks make many per-turn or per-file calls.
func teacherBrain() brain.Brain {
	bc := config.LoadConfig().Brain
	model := "sonnet"
	if bc.Provider == config.BrainClaudeCLI && bc.Model != "" {
		model = bc.Model
	}
	binary := bc.Binary
	if binary == "" {
		binary = "claude"
	}
	return brain.ClaudeCLI(binary, model, config.DefaultBrainTimeoutSeconds)
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
