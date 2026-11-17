//go:build taskeval && linux

package fixture

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"june/internal/db"
)

// The same seed must give the same world, and every person in one world must be a different person.
func TestBuildIsDeterministicWithDistinctPeople(t *testing.T) {
	for seed := int64(1); seed <= 50; seed++ {
		w := Build(seed)
		if !reflect.DeepEqual(w, Build(seed)) {
			t.Fatalf("seed %d: two builds differ", seed)
		}
		people := map[string]bool{}
		for _, p := range w.Projects {
			people[p.Owner] = true
		}
		for _, c := range w.Claims {
			people[c.Speaker] = true
		}
		if len(people) != len(w.Projects)+len(w.Claims) {
			t.Fatalf("seed %d: a person appears twice: %v", seed, people)
		}
	}
}

// Writing the world must put every project owner and claim speaker where the agent's memory search can reach it.
func TestWrittenWorldIsFoundByMemorySearch(t *testing.T) {
	store, err := db.New(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	w := Build(1)
	if err := w.Write(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	for _, p := range w.Projects {
		if !found(t, store, p.Name, p.Owner) {
			t.Errorf("search for %q did not return %q", p.Name, p.Owner)
		}
	}
	for _, c := range w.Claims {
		if !found(t, store, c.Topic, c.Speaker) {
			t.Errorf("search for %q did not return %q", c.Topic, c.Speaker)
		}
	}
}

func found(t *testing.T, store *db.Store, query, want string) bool {
	t.Helper()
	hits, err := store.HybridSearchWindow(context.Background(), query, "", time.Time{}, time.Time{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if strings.Contains(h.Content, want) {
			return true
		}
	}
	return false
}
