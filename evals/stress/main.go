// Command stress runs a list of questions through the live voice loop exactly as a typed turn would run (agent.AskVoice: real system prompt, real tools, the Live model), against a snapshot of the store so tool writes cannot touch live data, and prints one JSON trace per question. Input: questions on stdin, one per line. Output: JSON lines on stdout; slog on stderr with millisecond timestamps.
package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"ora/internal/agent"
	"ora/internal/config"
	"ora/internal/db"

	_ "modernc.org/sqlite"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	apiKey := os.Getenv("GEMINI_API_KEY")
	live := filepath.Join(config.DataDir(), "db")
	dir, err := os.MkdirTemp("", "ora-stress-db-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	dest := filepath.Join(dir, "db")
	src, err := sql.Open("sqlite", "file:"+live+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		panic(err)
	}
	if _, err := src.Exec("VACUUM INTO ?", dest); err != nil {
		panic(err)
	}
	src.Close()
	store, err := db.New(dest)
	if err != nil {
		panic(err)
	}
	defer store.Close()
	ag := agent.NewAgent(nil, nil, store, nil, apiKey)
	ag.AllowEvalWrites()
	sc := bufio.NewScanner(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	for sc.Scan() {
		q := sc.Text()
		if q == "" {
			continue
		}
		slog.Info("stress question", "q", q)
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		tr, err := ag.AskVoice(ctx, q)
		cancel()
		out := map[string]any{"question": q, "model": tr.Model, "duration_ms": tr.Duration.Milliseconds(), "hops": tr.ToolHops, "thoughts": tr.Thoughts, "answer": tr.Answer, "handshake_lines": len(tr.Handshake)}
		if err != nil {
			out["error"] = err.Error()
		}
		enc.Encode(out)
		fmt.Fprintln(os.Stderr)
	}
}
