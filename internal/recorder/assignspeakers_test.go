package recorder

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// sentenceWords returns n words, w0 w1 ..., each ending a sentence with probability stop.
func sentenceWords(rng *rand.Rand, n int, stop float64) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("w%d", i)
		if rng.Float64() < stop {
			out[i] += "."
		}
	}
	return out
}

// randomTimeline is up to eight voices taking turns between from and to, with silences between some turns and overlaps between others.
func randomTimeline(rng *rand.Rand, from, to time.Duration) []diarTurn {
	var turns []diarTurn
	for at := from; at < to; {
		length := seconds(0.2 + 9*rng.Float64())
		turns = append(turns, diarTurn{Start: at, End: at + length, Speaker: rng.Intn(1 + rng.Intn(8))})
		at += max(seconds(0.1), length+seconds(4*rng.Float64()-1))
	}
	return turns
}

// Whatever the segments and the timeline, the call side keeps every word once and in order, the microphone side is untouched, the labels are ones that fit the timeline, and the label only changes where a sentence ends.
func TestAssignSpeakers_KeepsWordsAndNeverChangesVoiceMidSentence(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	for range 300 {
		turns := randomTimeline(rng, 0, seconds(300))
		var segs []Segment
		var callWords []string
		at := time.Duration(0)
		for i, n := 0, 2+rng.Intn(8); i < n; i++ {
			end := at + seconds(1+34*rng.Float64())
			if rng.Intn(5) == 0 {
				segs = append(segs, Segment{Start: at, End: end, Speaker: speakerMe, Text: "mine"})
			} else {
				ws := sentenceWords(rng, 1+rng.Intn(80), 0.2)
				callWords = append(callWords, ws...)
				segs = append(segs, Segment{Start: at, End: end, Speaker: speakerCall, Text: strings.Join(ws, " ")})
			}
			at = end + seconds(3*rng.Float64())
		}
		heard := map[string]bool{speakerCall: true}
		for _, turn := range turns {
			heard[callSpeaker(turn.Speaker)] = true
		}

		got := assignSpeakers(append([]Segment{}, segs...), turns)

		var gotWords, mine, wantMine []string
		for _, s := range segs {
			if s.Speaker == speakerMe {
				wantMine = append(wantMine, s.Text)
			}
		}
		for i, s := range got {
			if s.End < s.Start || (i > 0 && s.Start < got[i-1].Start) {
				t.Fatalf("segment %v is out of order after %v", s, got[i-1])
			}
			if s.Speaker == speakerMe {
				mine = append(mine, s.Text)
				continue
			}
			if !heard[s.Speaker] {
				t.Fatalf("%v carries a label no turn gives", s)
			}
			gotWords = append(gotWords, strings.Fields(s.Text)...)
			if i+1 < len(got) && got[i+1].Speaker != speakerMe && got[i+1].Speaker != s.Speaker && !endsSentence(gotWords[len(gotWords)-1]) {
				t.Fatalf("the voice changes from %s to %s in the middle of a sentence ending %q", s.Speaker, got[i+1].Speaker, s.Text)
			}
		}
		if strings.Join(gotWords, " ") != strings.Join(callWords, " ") {
			t.Fatalf("the call's words changed: %q became %q", callWords, gotWords)
		}
		if strings.Join(mine, "|") != strings.Join(wantMine, "|") {
			t.Fatalf("the microphone segments changed: %q became %q", wantMine, mine)
		}
	}
}

// Two voices that shared a segment of one-word sentences get the share of the words that their share of the speaking time came to, with the silence between them not counted.
func TestAssignSpeakers_SharesSentencesByTimeSpoken(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	for range 200 {
		first, second := 1+20*rng.Float64(), 1+20*rng.Float64()
		silence := 5 * rng.Float64()
		n := 20 + rng.Intn(60)
		seg := Segment{Start: 0, End: seconds(first + silence + second), Speaker: speakerCall, Text: strings.Repeat("w. ", n)}
		turns := []diarTurn{{Start: 0, End: seconds(first), Speaker: 4}, {Start: seconds(first + silence), End: seconds(first + silence + second), Speaker: 7}}

		fromFirst := 0
		for _, s := range assignSpeakers([]Segment{seg}, turns) {
			if s.Speaker == callSpeaker(4) {
				fromFirst++
			}
		}
		if want := float64(n) * first / (first + second); float64(fromFirst) < want-1 || float64(fromFirst) > want+1 {
			t.Fatalf("%d words over %.1f s and %.1f s: the first voice got %d, want about %.1f", n, first, second, fromFirst, want)
		}
	}
}

// A sentence that whisper's segment boundary cut in two is one sentence with one voice, the one that spoke most of it.
func TestAssignSpeakers_KeepsASentenceCutBetweenSegmentsWhole(t *testing.T) {
	segs := []Segment{
		{Start: 0, End: seconds(10), Speaker: speakerCall, Text: "we should go to"},
		{Start: seconds(10), End: seconds(20), Speaker: speakerCall, Text: "the store tomorrow."},
	}
	turns := []diarTurn{{Start: 0, End: seconds(12), Speaker: 1}, {Start: seconds(12), End: seconds(20), Speaker: 2}}
	got := assignSpeakers(segs, turns)
	if len(got) != 1 || got[0].Speaker != callSpeaker(1) || got[0].Text != "we should go to the store tomorrow." {
		t.Errorf("got %v", got)
	}
}

// A segment that no turn touches keeps the pooled label and its text whole, and a short turn inside a long one does not take a sentence from it.
func TestAssignSpeakers_PooledWhenUnplacedAndBlipsDoNotCount(t *testing.T) {
	seg := Segment{Start: seconds(20), End: seconds(30), Speaker: speakerCall, Text: "okay then. fine."}
	if got := assignSpeakers([]Segment{seg}, []diarTurn{{Start: 0, End: seconds(5), Speaker: 1}}); len(got) != 1 || got[0].Speaker != speakerCall || got[0].Text != seg.Text {
		t.Errorf("an unplaced segment got %v", got)
	}
	blip := []diarTurn{{Start: seconds(20), End: seconds(30), Speaker: 1}, {Start: seconds(24), End: seconds(24.4), Speaker: 2}}
	for _, s := range assignSpeakers([]Segment{seg}, blip) {
		if s.Speaker != callSpeaker(1) {
			t.Errorf("a blip took %v", s)
		}
	}
}
