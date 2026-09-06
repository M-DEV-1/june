package memory_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"ora/internal/memory"
)

// --- fakes ---

type fakeDigester struct {
	callCount  int
	lastInput  []string
	inputSizes []int
	lastPrior  string
	priorSeen  []string
	result     string
	resultFrom func(prior string, summaries []string) string
	err        error
}

func (f *fakeDigester) Digest(ctx context.Context, prior string, summaries []string) (string, error) {
	f.callCount++
	f.lastInput = append([]string(nil), summaries...)
	f.inputSizes = append(f.inputSizes, len(summaries))
	f.lastPrior = prior
	f.priorSeen = append(f.priorSeen, prior)
	if f.resultFrom != nil {
		return f.resultFrom(prior, summaries), f.err
	}
	return f.result, f.err
}

type replaceCall struct {
	dayID      int64
	summaryIDs []int64
	digest     string
}

type fakeCompactStore struct {
	groups       []memory.SummaryGroup
	digests      map[int64]string
	replaceCalls []replaceCall
}

func (f *fakeCompactStore) ExistingDigest(ctx context.Context, dayID int64) (string, error) {
	return f.digests[dayID], nil
}

func (f *fakeCompactStore) OldSummaryGroups(ctx context.Context, olderThan time.Duration) ([]memory.SummaryGroup, error) {
	return f.groups, nil
}

func (f *fakeCompactStore) ReplaceSummariesWithDigest(ctx context.Context, dayID int64, summaryIDs []int64, digest string) error {
	f.replaceCalls = append(f.replaceCalls, replaceCall{dayID, summaryIDs, digest})
	if f.digests == nil {
		f.digests = map[int64]string{}
	}
	f.digests[dayID] = digest
	return nil
}

// --- tests ---

func TestCompactor_GroupsWithTwoSummaries_TriggersDigestAndReplace(t *testing.T) {
	digester := &fakeDigester{result: "merged daily digest"}
	store := &fakeCompactStore{
		groups: []memory.SummaryGroup{
			{
				DayID: 10,
				Day:   "2026-06-01",
				Summaries: []memory.NodeRef{
					{ID: 100, Content: "did some coding"},
					{ID: 101, Content: "reviewed a PR"},
				},
			},
		},
	}
	c := memory.NewCompactor(digester, store)

	if err := c.Compact(context.Background(), 7*24*time.Hour); err != nil {
		t.Fatalf("Compact returned error: %v", err)
	}

	if digester.callCount != 1 {
		t.Fatalf("expected 1 Digest call, got %d", digester.callCount)
	}
	if len(digester.lastInput) != 2 {
		t.Errorf("expected 2 summaries passed to Digest, got %d", len(digester.lastInput))
	}
	if len(store.replaceCalls) != 1 {
		t.Fatalf("expected 1 Replace call, got %d", len(store.replaceCalls))
	}
	rc := store.replaceCalls[0]
	if rc.dayID != 10 {
		t.Errorf("expected dayID=10, got %d", rc.dayID)
	}
	if len(rc.summaryIDs) != 2 || rc.summaryIDs[0] != 100 || rc.summaryIDs[1] != 101 {
		t.Errorf("unexpected summaryIDs: %v", rc.summaryIDs)
	}
	if rc.digest != "merged daily digest" {
		t.Errorf("unexpected digest: %q", rc.digest)
	}
}

func TestCompactor_SingleSummaryGroup_IsSkipped(t *testing.T) {
	digester := &fakeDigester{result: "should not be called"}
	store := &fakeCompactStore{
		groups: []memory.SummaryGroup{
			{
				DayID: 20,
				Day:   "2026-06-02",
				Summaries: []memory.NodeRef{
					{ID: 200, Content: "only one summary"},
				},
			},
		},
	}
	c := memory.NewCompactor(digester, store)

	if err := c.Compact(context.Background(), 7*24*time.Hour); err != nil {
		t.Fatalf("Compact returned error: %v", err)
	}

	if digester.callCount != 0 {
		t.Errorf("expected 0 Digest calls for single-summary group, got %d", digester.callCount)
	}
	if len(store.replaceCalls) != 0 {
		t.Errorf("expected 0 Replace calls for single-summary group, got %d", len(store.replaceCalls))
	}
}

func TestCompactor_DigestErrorOnOneGroup_DoesNotAbortOthers(t *testing.T) {
	store := &fakeCompactStore{
		groups: []memory.SummaryGroup{
			{
				DayID: 30,
				Day:   "2026-06-03",
				Summaries: []memory.NodeRef{
					{ID: 300, Content: "a"},
					{ID: 301, Content: "b"},
				},
			},
			{
				DayID: 31,
				Day:   "2026-06-04",
				Summaries: []memory.NodeRef{
					{ID: 310, Content: "c"},
					{ID: 311, Content: "d"},
				},
			},
		},
	}

	// errorOnFirstDigester errors on the first call (day 30) and succeeds on day 31.
	errorDigester := &errorOnFirstDigester{}
	c := memory.NewCompactor(errorDigester, store)
	err := c.Compact(context.Background(), 7*24*time.Hour)
	// Compact must not return an error even when one group fails.
	if err != nil {
		t.Errorf("Compact should not return error on per-group failure, got: %v", err)
	}

	// Second group must still have been processed.
	if len(store.replaceCalls) != 1 {
		t.Errorf("expected 1 Replace call (second group should succeed), got %d", len(store.replaceCalls))
	}
	if store.replaceCalls[0].dayID != 31 {
		t.Errorf("expected dayID=31 for successful group, got %d", store.replaceCalls[0].dayID)
	}
}

// errorOnFirstDigester errors on the first Digest call, succeeds on subsequent ones.
type errorOnFirstDigester struct {
	calls int
}

func (e *errorOnFirstDigester) Digest(ctx context.Context, prior string, summaries []string) (string, error) {
	e.calls++
	if e.calls == 1 {
		return "", errors.New("llm rate limit")
	}
	return "digest for group", nil
}

// A day digested a second time — normal, since a day's summaries cross the age cutoff over more than one run — must have its earlier digest shown to the model, so the paragraph that replaces it still covers the material the first one did.
func TestCompactor_PassesTheDaysExistingDigestIntoTheCall(t *testing.T) {
	digester := &fakeDigester{result: "both halves of the day"}
	store := &fakeCompactStore{
		groups: []memory.SummaryGroup{{
			DayID: 10,
			Day:   "2026-06-01",
			Summaries: []memory.NodeRef{
				{ID: 100, Content: "reviewed a PR"},
				{ID: 101, Content: "wrote the release notes"},
			},
		}},
		digests: map[int64]string{10: "the morning, already digested"},
	}

	if err := memory.NewCompactor(digester, store).Compact(context.Background(), 7*24*time.Hour); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if digester.lastPrior != "the morning, already digested" {
		t.Errorf("prior passed to Digest = %q, want the day's existing digest", digester.lastPrior)
	}
}

// A day can hold hundreds of summaries. All of them in one prompt runs past the model's input limit, the call fails, and the day is never digested at all — so they go in batches, each batch handed the digest the previous one produced.
func TestCompactor_DigestsALargeDayInBatches(t *testing.T) {
	const total = 120
	summaries := make([]memory.NodeRef, total)
	for i := range summaries {
		summaries[i] = memory.NodeRef{ID: int64(i + 1), Content: fmt.Sprintf("summary %d", i)}
	}

	digester := &fakeDigester{resultFrom: func(prior string, s []string) string {
		return fmt.Sprintf("digest covering %d more", len(s))
	}}
	store := &fakeCompactStore{
		groups: []memory.SummaryGroup{{DayID: 10, Day: "2026-06-01", Summaries: summaries}},
	}

	if err := memory.NewCompactor(digester, store).Compact(context.Background(), 7*24*time.Hour); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	if digester.callCount < 2 {
		t.Fatalf("expected the day split across more than one Digest call, got %d", digester.callCount)
	}
	for i, n := range digester.inputSizes {
		if n > total/2 {
			t.Errorf("batch %d held %d summaries, want the day split into smaller batches", i, n)
		}
	}
	if digester.priorSeen[0] != "" {
		t.Errorf("first batch saw prior %q, want empty — the day had no digest yet", digester.priorSeen[0])
	}
	if digester.priorSeen[1] != store.replaceCalls[0].digest {
		t.Errorf("second batch saw prior %q, want the first batch's digest %q", digester.priorSeen[1], store.replaceCalls[0].digest)
	}

	var replaced int
	for _, rc := range store.replaceCalls {
		replaced += len(rc.summaryIDs)
	}
	if replaced != total {
		t.Errorf("replaced %d summaries across the batches, want all %d", replaced, total)
	}
}
