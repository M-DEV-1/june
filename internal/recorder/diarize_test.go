package recorder

import (
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"
)

// randomTurns lays a random number of voices end to end on one timeline, each turn a random length and each voice given a random number of turns, so some voices speak for minutes and some for a second.
func randomTurns(rng *rand.Rand) []diarTurn {
	var turns []diarTurn
	at := time.Duration(0)
	for i, n := 0, 5+rng.Intn(60); i < n; i++ {
		length := time.Duration(200+rng.Intn(9000)) * time.Millisecond
		turns = append(turns, diarTurn{Start: at, End: at + length, Speaker: rng.Intn(1 + rng.Intn(8))})
		at += length + time.Duration(rng.Intn(3000))*time.Millisecond
	}
	return turns
}

func speechByVoice(turns []diarTurn) map[int]time.Duration {
	total := map[int]time.Duration{}
	for _, t := range turns {
		total[t.Speaker] += t.End - t.Start
	}
	return total
}

// Whatever the timeline, folding keeps every turn's times, leaves voices that already had enough speech alone, and leaves no voice below the minimum unless none reached it.
func TestFoldSmallClusters_KeepsOnlyVoicesWithEnoughSpeech(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for range 500 {
		turns := randomTurns(rng)
		original := slices.Clone(turns)
		before := speechByVoice(turns)
		anyLarge := false
		for _, d := range before {
			anyLarge = anyLarge || d >= minClusterSpeech
		}

		got := foldSmallClusters(turns)

		if !slices.Equal(turns, original) {
			t.Fatalf("the input was changed: %v", turns)
		}
		if len(got) != len(turns) {
			t.Fatalf("got %d turns from %d", len(got), len(turns))
		}
		for i := range got {
			if got[i].Start != turns[i].Start || got[i].End != turns[i].End {
				t.Fatalf("turn %d moved: %v became %v", i, turns[i], got[i])
			}
			if before[turns[i].Speaker] >= minClusterSpeech && got[i].Speaker != turns[i].Speaker {
				t.Fatalf("turn %d belongs to a voice with %s of speech and must not move: %v became %v", i, before[turns[i].Speaker], turns[i], got[i])
			}
		}
		if !anyLarge {
			if !slices.Equal(got, turns) {
				t.Fatalf("no voice reaches the minimum, so nothing has anywhere to fold into: %v became %v", turns, got)
			}
			continue
		}
		for voice, d := range speechByVoice(got) {
			if d < minClusterSpeech {
				t.Fatalf("voice %d is left with %s of speech: %v", voice, d, got)
			}
		}
	}
}

// A short voice goes to the larger voice that speaks next to it, not to the larger voice that speaks most.
func TestFoldSmallClusters_JoinsTheNearestLargerVoice(t *testing.T) {
	sec := func(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }
	turns := []diarTurn{
		{Start: sec(0), End: sec(60), Speaker: 0},
		{Start: sec(100), End: sec(120), Speaker: 1},
		{Start: sec(121), End: sec(122), Speaker: 2},
	}
	got := foldSmallClusters(turns)
	if got[2].Speaker != 1 {
		t.Errorf("a turn one second after voice 1 went to voice %d", got[2].Speaker)
	}
}

// writeSherpaStub writes a shell script that answers like the diarizer and returns the path of a bin beside which the two model files exist.
func writeSherpaStub(t *testing.T, output string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in diarizer is a shell script")
	}
	t.Setenv("JUNE_DATA_DIR", t.TempDir())
	dir := t.TempDir()
	bin := filepath.Join(dir, sherpaBinaryName)
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ncat <<'EOF'\n"+output+"EOF\n"), 0o755); err != nil {
		t.Fatalf("write the stand-in diarizer: %v", err)
	}
	return bin
}

// A count the caller supplied is exact, so a voice with little speech in it is a real person and stays; an estimated count has the small voice folded away.
func TestDiarizeWAV_FoldsOnlyAnEstimatedCount(t *testing.T) {
	bin := writeSherpaStub(t, "0.000 -- 60.000 speaker_00\n61.000 -- 62.000 speaker_01\n")
	voices := func(speakers int) int {
		turns, err := diarizeWAV(context.Background(), bin, "unused.wav", speakers, 0)
		if err != nil {
			t.Fatalf("diarizeWAV: %v", err)
		}
		return len(speechByVoice(turns))
	}
	if got := voices(2); got != 2 {
		t.Errorf("with a known count of two, got %d voices", got)
	}
	if got := voices(0); got != 1 {
		t.Errorf("with the count estimated, got %d voices", got)
	}
}
