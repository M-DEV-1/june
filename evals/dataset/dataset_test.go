package dataset

import (
	"strings"
	"testing"
)

func TestLoadQuestions_HasCurrentWork(t *testing.T) {
	qs, err := LoadQuestions()
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) == 0 {
		t.Fatal("no questions")
	}
	found := false
	for _, q := range qs {
		if q.ID == "current-work" {
			found = true
			if q.Ask == "" || len(q.Expect) == 0 {
				t.Fatalf("current-work incomplete: %+v", q)
			}
		}
		if q.Ask == "" || q.ID == "" {
			t.Fatalf("blank question: %+v", q)
		}
	}
	if !found {
		t.Fatal("missing current-work")
	}
}

func TestDistinctive_PicksLongTokens(t *testing.T) {
	got := distinctive("Driving Climate Risk Statement Builder ASRS for Opal HealthCare at Tingira Hills.", 4)
	if len(got) == 0 {
		t.Fatal("empty")
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "climate") && !strings.Contains(joined, "tingira") && !strings.Contains(joined, "healthcare") {
		t.Fatalf("got %v", got)
	}
}
