package main

import (
	"strings"
	"testing"
)

// TestGoldGrades checks the split the sheet leads with. A row whose source names the store the arms are being asked to search only measures whether an arm retrieved that row, so a wrong note scores as the right answer; only a row the user confirmed measures whether the answer is true. An unmarked row counts as retrieval, because store-derived is what a row is until somebody confirms it.
func TestGoldGrades(t *testing.T) {
	items := []goldItem{
		{ID: "A", Grade: "truth", Turns: []goldTurnSpec{{}}},
		{ID: "B", Grade: "retrieval", Turns: []goldTurnSpec{{}}},
		{ID: "C", Turns: []goldTurnSpec{{}}},
	}
	grades := goldGrades(items)
	if grades["A"] != goldTruth || grades["B"] != goldRetrieval || grades["C"] != goldRetrieval {
		t.Fatalf("goldGrades = %v, want A truth and B, C retrieval", grades)
	}

	answers := []goldAnswer{
		{ID: "A", Arm: "claude", Pass: true},
		{ID: "B", Arm: "claude", Pass: false},
		{ID: "C", Arm: "claude", Pass: true},
	}
	truth := goldTallyGraded(answers, grades, goldTruth)["claude"]
	if truth.Pass != 1 || truth.Fail != 0 {
		t.Fatalf("truth tally = %d pass, %d fail, want 1 and 0", truth.Pass, truth.Fail)
	}
	retrieval := goldTallyGraded(answers, grades, goldRetrieval)["claude"]
	if retrieval.Pass != 1 || retrieval.Fail != 1 {
		t.Fatalf("retrieval tally = %d pass, %d fail, want 1 and 1", retrieval.Pass, retrieval.Fail)
	}
}

// TestGoldSetIsGraded checks the shipped gold set: every row carries a grade, and a row is graded truth only when its source says the user confirmed it. Four rows exist because the store was wrong, and those are the only ones that measure correctness.
func TestGoldSetIsGraded(t *testing.T) {
	items, err := loadGold("gold/questions.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var truth []string
	for _, it := range items {
		switch it.Grade {
		case goldTruth:
			truth = append(truth, it.ID)
		case goldRetrieval:
		default:
			t.Errorf("%s carries no grade (%q)", it.ID, it.Grade)
		}
		userConfirmed := strings.HasPrefix(it.Source, "user, ")
		if userConfirmed != (it.Grade == goldTruth) {
			t.Errorf("%s: grade %q does not match its source %q", it.ID, it.Grade, it.Source)
		}
	}
	if len(truth) != 4 {
		t.Errorf("gold set has %d truth rows (%v), want the 4 the user confirmed", len(truth), truth)
	}
}
