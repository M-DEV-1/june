//go:build taskeval

// Package fixture builds a small invented world (people, projects, who said what) from a seed and writes it into a June store through the store's own write functions.
package fixture

import (
	"context"
	"fmt"
	"math/rand"

	"june/internal/db"
	"june/internal/memory"
)

// Project is one invented project and the person who owns it.
type Project struct{ Name, Owner string }

// Claim is one invented statement: who said that the named topic moved to the given date.
type Claim struct{ Speaker, Topic, Date string }

// World is the facts table for one seed. The suites grade answers against these fields, and Seed writes the same facts into a store.
type World struct {
	Seed     int64
	Projects []Project
	Claims   []Claim
	// OwnerQ, PhoneQ and ClaimQ pick which project, which owner and which claim the three memory questions ask about.
	OwnerQ, PhoneQ, ClaimQ int
}

var (
	firstNames = []string{"Marta", "Ilya", "Dana", "Tomas", "Priya", "Leif", "Noor", "Esme", "Anselm", "Odile", "Rurik", "Vesna", "Cato", "Linnea", "Joaquin", "Thea", "Bram", "Ines", "Kofi", "Sunniva"}
	lastNames  = []string{"Brightwater", "Hollowell", "Quillfeather", "Vandermoor", "Ashgrove", "Tallowby", "Pennywhistle", "Corbelline", "Marrowgate", "Fennimore", "Dunstable", "Larkhaven", "Orrinsby", "Wexcombe", "Thistledown", "Gallowmere", "Ravenscroft", "Pickersgill", "Stonebrook", "Yarrowby"}
	projects   = []string{"Larkspur", "Tamarind", "Quillon", "Basalt", "Cormorant", "Fennel", "Gannet", "Harrow", "Juniper", "Kestrel", "Lodestar", "Marlin", "Nettle", "Osprey", "Pennant", "Quarry"}
	topics     = []string{"vendor audit", "budget review", "security sign-off", "launch rehearsal", "hiring panel", "data migration", "pricing workshop", "offsite planning"}
	months     = []string{"January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"}
)

// Build returns the world for seed. The same seed always gives the same world: four projects with distinct owners and two claims by two other people.
func Build(seed int64) World {
	rng := rand.New(rand.NewSource(seed))
	first, last := rng.Perm(len(firstNames)), rng.Perm(len(lastNames))
	person := func(i int) string { return firstNames[first[i]] + " " + lastNames[last[i]] }

	w := World{Seed: seed}
	for i, p := range rng.Perm(len(projects))[:4] {
		w.Projects = append(w.Projects, Project{Name: "Project " + projects[p], Owner: person(i)})
	}
	for i, t := range rng.Perm(len(topics))[:2] {
		date := fmt.Sprintf("%s %d", months[rng.Intn(len(months))], 1+rng.Intn(28))
		w.Claims = append(w.Claims, Claim{Speaker: person(4 + i), Topic: topics[t], Date: date})
	}
	w.OwnerQ = rng.Intn(len(w.Projects))
	w.PhoneQ = rng.Intn(len(w.Projects))
	w.ClaimQ = rng.Intn(len(w.Claims))
	return w
}

// Write writes the world into store: a note and a thread per project, and a diary entry and a captured chat episode per claim. Output: an error from the first write that fails.
func (w World) Write(ctx context.Context, store *db.Store) error {
	for _, p := range w.Projects {
		if _, err := store.LogNote(ctx, fmt.Sprintf("%s is the owner of %s.", p.Owner, p.Name), "fact"); err != nil {
			return err
		}
		if _, err := store.UpsertThread(ctx, memory.ThreadUpdate{Subject: p.Name, Kind: "project", State: fmt.Sprintf("Owned by %s, work in progress.", p.Owner)}); err != nil {
			return err
		}
	}
	for i, c := range w.Claims {
		line := fmt.Sprintf("%s said the %s is moved to %s.", c.Speaker, c.Topic, c.Date)
		if err := store.SetDiaryEntry(ctx, fmt.Sprintf("2026-03-%02d", 2+i), "day", line); err != nil {
			return err
		}
		if _, err := store.LogEpisode(ctx, "Team Chat", "Planning channel", fmt.Sprintf("%s: the %s is moved to %s.", c.Speaker, c.Topic, c.Date)); err != nil {
			return err
		}
	}
	return nil
}
