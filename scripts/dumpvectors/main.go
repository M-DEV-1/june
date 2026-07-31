// dumpvectors prints documents from a chromem-go persistent store
// (ora-db/vectors) without the embedding vectors.
//
// Usage (from repo root):
//
//	go run ./scripts/dumpvectors
//	go run ./scripts/dumpvectors -path ora-db/vectors -collection memory
//	go run ./scripts/dumpvectors -json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	chromem "github.com/philippgille/chromem-go"
)

func main() {
	dbPath := flag.String("path", "ora-db/vectors", "chromem persistent DB directory")
	collection := flag.String("collection", "memory", "collection name")
	asJSON := flag.Bool("json", false, "emit one JSON object per document (no embeddings)")
	flag.Parse()

	if err := run(*dbPath, *collection, *asJSON); err != nil {
		fmt.Fprintf(os.Stderr, "dumpvectors: %v\n", err)
		os.Exit(1)
	}
}

type dumpDoc struct {
	ID            string            `json:"id"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	Content       string            `json:"content"`
	EmbeddingDims int               `json:"embedding_dims"`
}

func run(dbPath, collection string, asJSON bool) error {
	db, err := chromem.NewPersistentDB(dbPath, true)
	if err != nil {
		return fmt.Errorf("open chromem db %q: %w", dbPath, err)
	}

	col, err := db.GetOrCreateCollection(collection, nil, nil)
	if err != nil {
		return fmt.Errorf("get collection %q: %w", collection, err)
	}

	ids, err := loadIDs(dbPath, collection, col)
	if err != nil {
		return err
	}

	ctx := context.Background()
	docs := make([]dumpDoc, 0, len(ids))
	for _, id := range ids {
		doc, err := col.GetByID(ctx, id)
		if err != nil {
			return fmt.Errorf("get %q: %w", id, err)
		}
		docs = append(docs, dumpDoc{
			ID:            doc.ID,
			Metadata:      doc.Metadata,
			Content:       doc.Content,
			EmbeddingDims: len(doc.Embedding),
		})
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		for _, d := range docs {
			if err := enc.Encode(d); err != nil {
				return err
			}
		}
		return nil
	}

	fmt.Printf("db:         %s\n", dbPath)
	fmt.Printf("collection: %s\n", collection)
	fmt.Printf("count:      %d (chromem=%d)\n\n", len(docs), col.Count())

	for i, d := range docs {
		fmt.Printf("─── [%d] %s ───\n", i+1, d.ID)
		if len(d.Metadata) > 0 {
			keys := make([]string, 0, len(d.Metadata))
			for k := range d.Metadata {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Printf("  %s: %s\n", k, d.Metadata[k])
			}
		}
		fmt.Printf("  embedding_dims: %d\n", d.EmbeddingDims)
		fmt.Printf("  content:\n%s\n\n", indent(d.Content, "    "))
	}
	return nil
}

// loadIDs prefers ora's sidecar (collection_meta.json).
// If missing or empty, falls back to a nearest-neighbor dump of the whole collection (dummy query).
func loadIDs(dbPath, collection string, col *chromem.Collection) ([]string, error) {
	sidecar := filepath.Join(dbPath, collection+"_meta.json")
	if data, err := os.ReadFile(sidecar); err == nil {
		var createdAt map[string]string
		if err := json.Unmarshal(data, &createdAt); err != nil {
			return nil, fmt.Errorf("parse sidecar %s: %w", sidecar, err)
		}
		ids := make([]string, 0, len(createdAt))
		for id := range createdAt {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		if len(ids) > 0 {
			return ids, nil
		}
	}

	n := col.Count()
	if n == 0 {
		return nil, nil
	}

	// Can't discover the embedding dim without IDs, so probe by querying with zero vectors of common Gemini embedding sizes until one works.
	ctx := context.Background()
	for _, dim := range []int{768, 3072, 1536, 1024, 384} {
		dummy := make([]float32, dim)
		results, err := col.QueryEmbedding(ctx, dummy, n, nil, nil)
		if err != nil {
			// wrong dim or empty — try next
			if strings.Contains(err.Error(), "dimensions") ||
				strings.Contains(err.Error(), "length") ||
				strings.Contains(err.Error(), "embedding") {
				continue
			}
			// some other error (e.g. nResults) — still try
			continue
		}
		ids := make([]string, 0, len(results))
		for _, r := range results {
			ids = append(ids, r.ID)
		}
		sort.Strings(ids)
		return ids, nil
	}

	return nil, fmt.Errorf("no sidecar at %s and could not probe collection embeddings; collection count=%d", sidecar, n)
}

func indent(s, prefix string) string {
	if s == "" {
		return prefix + "(empty)"
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}
