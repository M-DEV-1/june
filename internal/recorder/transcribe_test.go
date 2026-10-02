package recorder

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeWhisper returns a stand-in for whisper-cli that prints stdout and stderr and exits 0, with no model beside it.
func fakeWhisper(t *testing.T, stdout, stderr string) string {
	t.Helper()
	t.Setenv("FAKE_WHISPER_STDOUT", stdout)
	t.Setenv("FAKE_WHISPER_STDERR", stderr)
	return fakeWhisperBin(t, false)
}

// testWAV writes a small valid recording so repairWAV has something to open.
func testWAV(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mic.wav")
	w, err := newWAV(path)
	if err != nil {
		t.Fatalf("newWAV: %v", err)
	}
	if _, err := w.Write(make([]byte, 1600)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return path
}

// A silent clip is a real outcome, not a failure: whisper-cli exits nonzero when it cannot read or decode the audio, so an empty result with a clean exit is a meeting nobody spoke in. Stderr chatter must not cost us the transcript either: whisper prints progress and banners to stderr on every successful run too.
func TestTranscribeWAV_SilentClipIsNotAnError(t *testing.T) {
	cases := []struct {
		name     string
		stdout   string
		stderr   string
		wantLen  int
		wantText string
	}{
		{name: "a clean exit with nothing printed is silence, not an error"},
		{
			name:     "stderr noise on a successful run does not cost the transcript",
			stdout:   "[00:00:00.000 --> 00:00:01.000]   hello there",
			stderr:   "whisper_model_load: model size = 487.01 MB",
			wantLen:  1,
			wantText: "hello there",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bin := fakeWhisper(t, c.stdout, c.stderr)
			segs, err := transcribeWAV(context.Background(), bin, testWAV(t), speakerMe, "", 0)
			if err != nil {
				t.Fatalf("transcribeWAV: %v", err)
			}
			if len(segs) != c.wantLen {
				t.Fatalf("got %+v, want %d segments", segs, c.wantLen)
			}
			if c.wantLen == 1 && segs[0].Text != c.wantText {
				t.Errorf("got %+v, want text %q", segs, c.wantText)
			}
		})
	}
}

// whisper prints one line per segment as "[start --> end]   text". Everything else — banners, blank lines, a fractional timestamp of however many digits the build chose to print, a stream's own offset from the recording's start, and a bracketed non-speech marker — has to be read correctly or dropped outright.
func TestParseSegments(t *testing.T) {
	cases := []struct {
		name    string
		out     string
		speaker string
		offset  time.Duration
		check   func(t *testing.T, segs []Segment)
	}{
		{
			name: "one line per segment, banners and junk lines are not segments",
			out: `
[00:00:00.000 --> 00:00:07.640]   This is a LibriVox recording.
[00:00:07.640 --> 00:01:17.840]   For more information please visit librivox.org.
not a segment line
[01:02:03.500 --> 01:02:04.000]
`,
			speaker: speakerMe,
			check: func(t *testing.T, segs []Segment) {
				if len(segs) != 2 {
					t.Fatalf("got %d segments, want 2 (blank text and junk lines dropped): %+v", len(segs), segs)
				}
				if segs[0].Start != 0 || segs[0].End != 7640*time.Millisecond {
					t.Errorf("segment 0 span = %v..%v, want 0..7.64s", segs[0].Start, segs[0].End)
				}
				if segs[0].Text != "This is a LibriVox recording." {
					t.Errorf("segment 0 text = %q", segs[0].Text)
				}
				if segs[1].Start != 7640*time.Millisecond || segs[1].End != 77840*time.Millisecond {
					t.Errorf("segment 1 span = %v..%v, want 7.64s..1m17.84s", segs[1].Start, segs[1].End)
				}
				if segs[0].Speaker != speakerMe {
					t.Errorf("speaker = %q, want %q", segs[0].Speaker, speakerMe)
				}
			},
		},
		{
			// The fractional part of a whisper timestamp is however many digits the build chose to print, so its length sets its scale: ".06" is 60ms, not 6ms, and ".5" is half a second, not half a millisecond.
			name:    "the fractional part of a timestamp scales by its digit count",
			out:     "[00:00:00.06 --> 00:00:01.5]   hello",
			speaker: speakerMe,
			check: func(t *testing.T, segs []Segment) {
				if len(segs) != 1 {
					t.Fatalf("got %d segments, want 1", len(segs))
				}
				if segs[0].Start != 60*time.Millisecond {
					t.Errorf("start = %v, want 60ms", segs[0].Start)
				}
				if segs[0].End != 1500*time.Millisecond {
					t.Errorf("end = %v, want 1.5s", segs[0].End)
				}
			},
		},
		{
			// The two streams do not open at the same instant, so each stream's segments are shifted by how late that stream started relative to the recording as a whole.
			name:    "a stream offset shifts every segment in it",
			out:     "[00:00:01.000 --> 00:00:02.000]   hello",
			speaker: speakerCall,
			offset:  500 * time.Millisecond,
			check: func(t *testing.T, segs []Segment) {
				if len(segs) != 1 {
					t.Fatalf("got %d segments, want 1", len(segs))
				}
				if segs[0].Start != 1500*time.Millisecond {
					t.Errorf("start = %v, want 1.5s", segs[0].Start)
				}
				if segs[0].End != 2500*time.Millisecond {
					t.Errorf("end = %v, want 2.5s", segs[0].End)
				}
			},
		},
		{
			// whisper marks silence and non-speech sound as a bracketed pseudo-segment. One side of a call is silent most of the time, so those lines would otherwise be most of the transcript.
			name:    "non-speech markers are dropped",
			out:     "[00:00:00.000 --> 00:00:07.000]   [BLANK_AUDIO]\n[00:00:07.000 --> 00:00:08.000]   (upbeat music)\n[00:00:08.000 --> 00:00:09.000]   real words here",
			speaker: speakerMe,
			check: func(t *testing.T, segs []Segment) {
				if len(segs) != 1 || segs[0].Text != "real words here" {
					t.Errorf("expected only the spoken segment, got %+v", segs)
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.check(t, parseSegments(c.out, c.speaker, c.offset))
		})
	}
}

// fakeWhisperLogging is fakeWhisper with a model file beside it, so whisperCPPArgs treats it as a real whisper.cpp build, and with every invocation's arguments appended to a log file. Input: what the fake should print to stdout. Output: the fake's path and the path of the argument log, one line per run.
func fakeWhisperLogging(t *testing.T, stdout string) (bin, argLog string) {
	t.Helper()
	argLog = filepath.Join(t.TempDir(), "args.log")
	t.Setenv("FAKE_WHISPER_STDOUT", stdout)
	t.Setenv("FAKE_WHISPER_ARGLOG", argLog)
	return fakeWhisperBin(t, true), argLog
}

// runArgs returns the arguments of each run the fake whisper logged, in order.
func runArgs(t *testing.T, argLog string) []string {
	t.Helper()
	b, err := os.ReadFile(argLog)
	if err != nil {
		t.Fatalf("read the fake whisper's argument log: %v", err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// A run that loops on a silence marker is redone without the prompt, and the redo has to keep everything else the first run had — above all the model flags, without which whisper-cli looks for a model that is not there and the whole meeting fails.
func TestTranscribeWAV_TheUnprimedRetryKeepsTheModelFlags(t *testing.T) {
	looped := "[00:00:00.000 --> 00:00:01.000]   [ Silence ]\n[00:00:01.000 --> 00:00:02.000]   [ Silence ]\n[00:00:02.000 --> 00:00:03.000]   hello"
	bin, argLog := fakeWhisperLogging(t, looped)

	if _, err := transcribeWAV(context.Background(), bin, testWAV(t), speakerMe, "Participants: Vexil Quorin.", 0); err != nil {
		t.Fatalf("transcribeWAV: %v", err)
	}

	runs := runArgs(t, argLog)
	if len(runs) != 2 {
		t.Fatalf("whisper ran %d times, want 2: the primed run and the unprimed redo\n%v", len(runs), runs)
	}
	if !strings.Contains(runs[0], "--prompt") {
		t.Errorf("the first run carried no prompt: %s", runs[0])
	}
	if strings.Contains(runs[1], "--prompt") {
		t.Errorf("the redo still carried the prompt it was redone to drop: %s", runs[1])
	}
	for i, args := range runs {
		if !strings.Contains(args, "-m ") {
			t.Errorf("run %d lost the model flags, so whisper has no model to load: %s", i, args)
		}
	}
}

// fakeWhisperDyingOnTheGPUFirst returns a stand-in for whisper-cli, with a model beside it so the run takes the GPU path, that dies the way the real one died on 2026-09-06 (the Vulkan allocation failure on stderr) on its first run and prints stdout on every run after that. Output: its path and the path of the log each run appends its arguments to.
func fakeWhisperDyingOnTheGPUFirst(t *testing.T, stdout string) (bin, argLog string) {
	t.Helper()
	bin, argLog = fakeWhisperLogging(t, stdout)
	t.Setenv("FAKE_WHISPER_RAN", filepath.Join(t.TempDir(), "ran"))
	t.Setenv("FAKE_WHISPER_DIE", "once")
	return bin, argLog
}

// The card is shared with the embedding server and a shadow model, so a decode can find no memory on it and die mid-run. The same audio decodes on the CPU, so the run is redone there instead of the words being thrown away.
func TestTranscribeWAV_RetriesOnTheCPUWhenTheGPURunDies(t *testing.T) {
	bin, argLog := fakeWhisperDyingOnTheGPUFirst(t, "[00:00:00.000 --> 00:00:01.000]   shall we ship on friday")

	segs, err := transcribeWAV(context.Background(), bin, testWAV(t), speakerMe, "", 0)
	if err != nil {
		t.Fatalf("transcribeWAV: %v", err)
	}
	if len(segs) != 1 || segs[0].Text != "shall we ship on friday" {
		t.Fatalf("got %+v, want the segment the CPU retry transcribed", segs)
	}
	runs := runArgs(t, argLog)
	if len(runs) != 2 {
		t.Fatalf("whisper ran %d times, want 2: the GPU run that died and the CPU retry\n%v", len(runs), runs)
	}
	if strings.Contains(runs[0], "-ng") {
		t.Errorf("the first run was already on the CPU: %s", runs[0])
	}
	if !strings.Contains(runs[1], "-ng") {
		t.Errorf("the retry did not turn the GPU off, so it fails for the same reason: %s", runs[1])
	}
	if !strings.Contains(runs[1], "-m ") {
		t.Errorf("the retry lost the model flags, so whisper has no model to load: %s", runs[1])
	}
}
