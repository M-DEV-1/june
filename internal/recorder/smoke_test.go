//go:build linux

package recorder

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jfreymuth/pulse"
	"github.com/jfreymuth/pulse/proto"

	"ora/internal/config"
	"ora/internal/db"
)

// TestSmoke_RealCapture is the manual end-to-end check: it plays a known speech WAV through the default sink, records it off the sink's monitor, and runs the real whisper binary over the result.
// It needs a running sound server, a whisper.cpp install, and a person to have set ORA_SMOKE_WAV to a 16 kHz mono s16le speech file, so it is skipped by default.
//
//	ORA_SMOKE_WAV=/path/to/speech.wav go test ./internal/recorder/ -run Smoke -v -timeout 30m
func TestSmoke_RealCapture(t *testing.T) {
	wav := os.Getenv("ORA_SMOKE_WAV")
	if wav == "" {
		t.Skip("set ORA_SMOKE_WAV to a 16 kHz mono s16le speech file to run the real capture smoke test")
	}

	dataDir := t.TempDir()
	// Screen context shaped like what the tracker actually captures from a Google Meet tab, so the run also shows whether the model puts those names to the pooled [call] voice.
	store := &fakeStore{episodes: []db.Episode{{
		CreatedAt:  time.Now(),
		App:        "Brave Browser",
		Title:      "Meet – poetry review - Brave",
		ScreenText: "participating in a video call | poetry review | Chris Scorringe (Presenting) | Alex Rivera",
	}}}
	r := New(dataDir, store, os.Getenv("GEMINI_API_KEY"))
	// With no API key the summary half cannot run, but the audio half is still worth proving, so stub the model out in that case only.
	if os.Getenv("GEMINI_API_KEY") == "" {
		r.minutes = func(ctx context.Context, prompt string) (string, error) {
			return "# Minutes\n(stubbed, no API key)", nil
		}
	}

	// The recording itself goes to a temp dir, but whisper is looked up where it is really installed.
	r.findWhisper = func(string) (string, error) { return whisperCPPBinary(config.DataDir()) }

	if err := r.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := playToDefaultSink(wav, 20*time.Second); err != nil {
		t.Fatalf("playback: %v", err)
	}
	s, err := r.stop()
	if err != nil {
		t.Fatalf("stop: %v", err)
	}

	for _, name := range []string{"mic.wav", "system.wav"} {
		info, err := os.Stat(filepath.Join(s.dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if info.Size() <= wavHeaderSize {
			t.Errorf("%s captured no samples (%d bytes)", name, info.Size())
		}
	}

	start := time.Now()
	if err := r.process(context.Background(), s); err != nil {
		t.Fatalf("process: %v", err)
	}
	t.Logf("transcription + summary took %v", time.Since(start))

	transcript, err := os.ReadFile(filepath.Join(s.dir, "transcript.md"))
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}
	t.Logf("transcript:\n%s", transcript)
	if !strings.Contains(string(transcript), "["+speakerCall+"]") {
		t.Error("the sink monitor stream produced no speech — system audio was not captured")
	}

	minutes, err := os.ReadFile(filepath.Join(s.dir, "minutes.md"))
	if err != nil {
		t.Fatalf("minutes: %v", err)
	}
	t.Logf("minutes:\n%s", minutes)
	if len(store.notes) != 1 {
		t.Errorf("expected the minutes filed as one note, got %d", len(store.notes))
	}
}

// playToDefaultSink streams at most d of a 16 kHz mono s16le WAV to the default output, which is what the recorder's monitor stream then hears.
func playToDefaultSink(path string, d time.Duration) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	raw = raw[wavHeaderSize:]
	if want := int(d.Seconds()) * sampleRate * 2; want < len(raw) {
		raw = raw[:want]
	}

	c, err := pulse.NewClient(pulse.ClientApplicationName("ora meeting smoke test"))
	if err != nil {
		return err
	}
	defer c.Close()

	pos := 0
	done := make(chan struct{})
	stream, err := c.NewPlayback(pulse.Int16Reader(func(out []int16) (int, error) {
		n := 0
		for n < len(out) && pos+1 < len(raw) {
			out[n] = int16(binary.LittleEndian.Uint16(raw[pos:]))
			pos += 2
			n++
		}
		if n == 0 {
			close(done)
			return 0, pulse.EndOfData
		}
		return n, nil
	}), pulse.PlaybackSampleRate(sampleRate), pulse.PlaybackChannels(proto.ChannelMap{proto.ChannelMono}), pulse.PlaybackLatency(0.2))
	if err != nil {
		return err
	}
	stream.Start()
	<-done
	stream.Drain()
	stream.Close()
	return nil
}
