//go:build linux

package audio

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"testing"
)

const testChunk = 1200 // 50 ms at 24 kHz, what the capture callback delivers

// tone returns n samples of a sine whose RMS is rms, over a whole number of cycles so the RMS is exact.
func tone(n int, rms float64) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(math.Round(rms * math.Sqrt2 * math.Sin(2*math.Pi*float64(i)*100/float64(n))))
	}
	return out
}

func TestEchoGuard_MutesOnlyTheFirstSecondAfterOpen(t *testing.T) {
	g := newEchoGuard()
	for i := 0; i < 2*micOpenMuteSamples/testChunk; i++ {
		silenced := g.process(tone(testChunk, 3000), 0)
		if want := i < micOpenMuteSamples/testChunk; silenced != want {
			t.Fatalf("chunk %d: silenced = %v, want %v", i, silenced, want)
		}
	}
}

func TestEchoGuard_SilencesChunksQuieterThanTheRatioToPlayback(t *testing.T) {
	for _, playback := range []float64{500, 2000, 8000, 20000} {
		for _, ratio := range []float64{0.01, 0.05, 0.1, 0.2, 0.24, 0.26, 0.5, 1, 3} {
			g := &echoGuard{}
			chunk := tone(testChunk, ratio*playback)
			silenced := g.process(chunk, playback)
			if want := ratio <= echoRatio; silenced != want {
				t.Errorf("playback %.0f, mic at %.2f of it: silenced = %v, want %v", playback, ratio, silenced, want)
			}
			if silenced && sampleRMS(chunk) != 0 {
				t.Errorf("playback %.0f, mic at %.2f of it: silenced chunk still has sound", playback, ratio)
			}
		}
	}
}

func TestEchoGuard_ReferenceFadesAfterPlaybackStops(t *testing.T) {
	g := &echoGuard{}
	const playback = 10000.0
	g.process(tone(testChunk, playback), playback)
	if !g.process(tone(testChunk, 0.1*playback), 0) {
		t.Error("echo right after playback stopped was passed")
	}
	for i := 0; i < 20; i++ {
		g.process(tone(testChunk, 0), 0)
	}
	if g.process(tone(testChunk, 0.1*playback), 0) {
		t.Error("a quiet sound a second after playback stopped was still treated as echo")
	}
}

func TestTakePlaybackPeak_ReturnsTheLoudestWriteThenResets(t *testing.T) {
	takePlaybackPeak()
	for _, rms := range []float64{3, 5, 4} {
		notePlayback(rms)
	}
	if got := takePlaybackPeak(); got != 5 {
		t.Errorf("peak = %v, want 5", got)
	}
	if got := takePlaybackPeak(); got != 0 {
		t.Errorf("peak after taking = %v, want 0", got)
	}
}

// Replays a recorded session through the guard. testdata/echoguard_replay.s16 is the echo-cancelled mic as 24 kHz s16: 48 chunks recorded while the assistant spoke and nobody else did, then 24 chunks of a person speaking with nothing playing. testdata/echoguard_replay.txt has one line per chunk: which of the two it is and the loudest playback RMS the speaker wrote in the 50 ms before the chunk arrived. Each kind runs through its own guard.
func TestEchoGuard_RecordedSession(t *testing.T) {
	raw, err := os.ReadFile("testdata/echoguard_replay.s16")
	if err != nil {
		t.Fatal(err)
	}
	pcm := make([]int16, len(raw)/2)
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(raw[i*2:]))
	}
	f, err := os.Open("testdata/echoguard_replay.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	guards := map[string]*echoGuard{"echo": {}, "user": {}}
	passed, total, lostSpeech := map[string]int{}, map[string]int{}, 0
	lines := bufio.NewScanner(f)
	for i := 0; lines.Scan(); i++ {
		var kind string
		var playback float64
		if _, err := fmt.Sscan(lines.Text(), &kind, &playback); err != nil {
			t.Fatal(err)
		}
		total[kind]++
		chunk := pcm[i*testChunk : (i+1)*testChunk]
		hadSound := sampleRMS(chunk) != 0
		if !guards[kind].process(chunk, playback) {
			passed[kind]++
		} else if kind == "user" && hadSound {
			lostSpeech++
		}
	}
	if passed["echo"]*10 > total["echo"] {
		t.Errorf("echo chunks passed: %d of %d, want at most 10%%", passed["echo"], total["echo"])
	}
	if lostSpeech != 0 {
		t.Errorf("%d speech chunks with sound were silenced while nothing was playing, want 0", lostSpeech)
	}
}
