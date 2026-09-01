package db

import (
	"strings"
	"testing"
)

// Half of every character Ora ever captured has no vector: an episode is embedded as one document capped at maxEmbedRunes, so a 96,061-character screen is represented by its first 4,000 and the rest is unreachable by meaning. Chunking makes the whole capture addressable.
func TestChunkText_CoversTheWholeCaptureNotJustItsHead(t *testing.T) {
	// A long screen whose answer sits near the end — the shape that made "what were the eleven findings" unanswerable.
	body := strings.Repeat("navigation chrome and boilerplate. ", 400)
	text := body + "\nthe eleven findings, two of them high severity, IPC auth\n" + body

	chunks := chunkText(text, chunkRunes, chunkOverlap)

	if len(chunks) < 2 {
		t.Fatalf("got %d chunk(s) for %d runes; a long capture must split", len(chunks), len([]rune(text)))
	}
	var found bool
	for _, c := range chunks {
		if strings.Contains(c, "eleven findings") {
			found = true
		}
		if n := len([]rune(c)); n > chunkRunes {
			t.Errorf("chunk of %d runes exceeds the embedder's budget of %d", n, chunkRunes)
		}
	}
	if !found {
		t.Error("the answer fell between chunks — overlap is meant to prevent exactly that")
	}
}

// Most captures are short: the average is 1,528 runes. Those must stay one chunk, so the common case gains an id suffix and nothing else.
func TestChunkText_LeavesAShortCaptureWhole(t *testing.T) {
	text := "Brave Browser · a short screen with one idea on it"
	chunks := chunkText(text, chunkRunes, chunkOverlap)
	if len(chunks) != 1 || chunks[0] != text {
		t.Errorf("got %d chunks %q, want the text unchanged", len(chunks), chunks)
	}
}

// An empty or whitespace-only capture yields nothing to embed rather than one empty chunk the embedder would reject.
func TestChunkText_YieldsNothingForEmptyText(t *testing.T) {
	if got := chunkText("   \n\t ", chunkRunes, chunkOverlap); len(got) != 0 {
		t.Errorf("got %d chunks, want none", len(got))
	}
}

// A chunk's vector id carries a "#N" suffix, and the search path parses ids back to rows to recover an episode's app, title and domain. Left unhandled, a passage hit parses to refID 0 and arrives detached from the screen it came from.
func TestSplitCandidateID_ResolvesAPassageToItsEpisode(t *testing.T) {
	for _, id := range []string{"episode:42", "episode:42#3"} {
		source, refID := splitCandidateID(id)
		if source != "episode" || refID != 42 {
			t.Errorf("%s parsed to (%q, %d), want (episode, 42)", id, source, refID)
		}
	}
}

// Two passages of one screen are two vectors and would arrive as two hits for the same row — the same capture twice in a ten-row budget. Only the best-scoring passage of a row survives fusion.
func TestBestPassagePerRow_KeepsOneHitPerCapture(t *testing.T) {
	in := []rrfCandidate{
		{id: "episode:42#2", score: 0.9},
		{id: "episode:42", score: 0.4},
		{id: "note:7", score: 0.7},
		{id: "episode:9#1", score: 0.6},
	}
	got := bestPassagePerRow(in)

	if len(got) != 3 {
		t.Fatalf("got %d candidates, want 3 distinct rows", len(got))
	}
	for _, c := range got {
		if c.id == "episode:42" {
			t.Error("kept the weaker passage of episode 42; the best-scoring one should win")
		}
	}
}
