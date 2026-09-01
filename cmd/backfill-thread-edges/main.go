// Command backfill-thread-edges rebuilds the episode-to-thread links for flushes that happened before the compiler started recording them.
//
// The compiler always knew which episodes a thread was attributed from — it hands a buffer to the model and gets back the threads it belongs to — and until 2026-09-01 it discarded that on every flush. The result was a store with 261 threads, 4,901 episodes and nothing joining them, so a thread could say "reviewed the code, eleven findings" and no query could reach the screens the findings were on.
//
// The links are recoverable because the compiler left a trace: one summary node per thread per flush, carrying the thread's subject and the moment of the flush. Those timestamps are the flush boundaries, so this rebuilds the same windows the compiler used rather than guessing at them.
//
// Safe to re-run: the link table ignores duplicates. Episodes older than the oldest summary node stay unlinked, because nothing on disk records what they were attributed to.
//
//	go run ./cmd/backfill-thread-edges [-n]
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"ora/internal/config"
	"ora/internal/db"
)

func main() {
	dry := flag.Bool("n", false, "report what is there and change nothing")
	flag.Parse()

	path := filepath.Join(config.DataDir(), "db")
	store, err := db.New(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open %s: %v\n", path, err)
		os.Exit(1)
	}
	defer store.Close()

	ctx := context.Background()
	before, err := store.CountThreadEdges(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "count edges: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("edges before: %d\n", before)
	if *dry {
		fmt.Println("dry run — nothing written")
		return
	}

	flushes, err := store.BackfillThreadEdges(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "backfill: %v\n", err)
		os.Exit(1)
	}
	after, err := store.CountThreadEdges(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "count edges: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("flushes replayed: %d\nedges after: %d (+%d)\n", flushes, after, after-before)
}
