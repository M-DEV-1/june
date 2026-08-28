package recorder

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// parakeetStdout is real output from sherpa-onnx-vad-with-offline-asr over the first minute of a kept meeting recording, captured on 2026-08-28. The banner lines the tool prints go to stderr, so stdout is nothing but segments.
const parakeetStdout = `2.758 -- 4.140: Yes, can you hear me?
6.822 -- 7.180: Yeah.
32.806 -- 33.996: Are you sharing something?
47.878 -- 49.804: Oh I realized
50.278 -- 51.020: Um
52.934 -- 57.612: Yeah, I need to do a little more analysis on on on what you've done.`

// fakeParakeet writes a shell script standing in for sherpa-onnx-vad-with-offline-asr, printing stdout to stdout and stderr to stderr and exiting 0. The model files are written alongside it because transcribeParakeetWAV finds them next to the binary.
func fakeParakeet(t *testing.T, stdout, stderr string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range parakeetModelFiles {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	path := filepath.Join(dir, parakeetBinaryName)
	body := "#!/bin/sh\ncat <<'EOF'\n" + stdout + "\nEOF\ncat <<'EOF' >&2\n" + stderr + "\nEOF\nexit 0\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake parakeet: %v", err)
	}
	return path
}

// sherpa-onnx prints one line per VAD-cut segment as "start -- end: text", with the times in seconds from the start of the file. Every timestamp is shifted by the stream's offset so both sides of a call land on one clock.
func TestParseParakeetSegments(t *testing.T) {
	segs := parseParakeetSegments(parakeetStdout, speakerCall, 2*time.Second)
	if len(segs) != 6 {
		t.Fatalf("got %d segments, want 6: %+v", len(segs), segs)
	}
	first := segs[0]
	if first.Start != 4758*time.Millisecond || first.End != 6140*time.Millisecond {
		t.Errorf("first segment spans %s to %s, want 4.758s to 6.140s after the offset", first.Start, first.End)
	}
	if first.Speaker != speakerCall {
		t.Errorf("speaker = %q, want %q", first.Speaker, speakerCall)
	}
	if first.Text != "Yes, can you hear me?" {
		t.Errorf("text = %q", first.Text)
	}
}

// Anything that is not a "start -- end: text" line is not a segment. sherpa-onnx keeps its banners on stderr, but a line that merely contains a colon or a number must never be read as speech.
func TestParseParakeetSegments_IgnoresEverythingThatIsNotASegment(t *testing.T) {
	out := `Creating recognizer ...
num threads: 4
decoding method: greedy_search
Elapsed seconds: 1.639 s
Real time factor (RTF): 1.639 / 60.000 = 0.027
1.000 -- 2.000: real speech

`
	segs := parseParakeetSegments(out, speakerMe, 0)
	if len(segs) != 1 || segs[0].Text != "real speech" {
		t.Fatalf("got %+v, want only the one spoken segment", segs)
	}
}

// The repeat-collapse guard is whisper's, but it stays on for parakeet too: a transducer loops far less often, not never, and the guard costs nothing on a transcript with no loop in it.
func TestParseParakeetSegments_StillCollapsesRepeats(t *testing.T) {
	out := `1.000 -- 2.000: Thanks for watching this video.
3.000 -- 4.000: Thanks for watching this video.
5.000 -- 6.000: Thanks for watching this video.
7.000 -- 8.000: something someone really said`
	segs := parseParakeetSegments(out, speakerMe, 0)
	if len(segs) != 1 || segs[0].Text != "something someone really said" {
		t.Fatalf("got %+v, want the looped sentence dropped and the real one kept", segs)
	}
}

// A segment whose text is only a bracketed marker is not speech, the same as it is not for whisper.
func TestParseParakeetSegments_DropsNonSpeechMarkers(t *testing.T) {
	segs := parseParakeetSegments("1.000 -- 2.000: [BLANK_AUDIO]\n3.000 -- 4.000: words\n", speakerMe, 0)
	if len(segs) != 1 || segs[0].Text != "words" {
		t.Fatalf("got %+v, want only the spoken segment", segs)
	}
}

// parakeetBinary finds the runner next to its models under dataDir/parakeet, and says so plainly when the engine was asked for but never installed.
func TestParakeetBinary(t *testing.T) {
	dir := t.TempDir()
	if _, err := parakeetBinary(dir); err == nil {
		t.Fatal("an empty data directory must not yield a parakeet binary")
	}

	pkDir := filepath.Join(dir, "parakeet")
	if err := os.MkdirAll(pkDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	bin := filepath.Join(pkDir, parakeetBinaryName)
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	// The binary alone is not enough: without the model files beside it every run would fail deep inside sherpa-onnx instead of here.
	if _, err := parakeetBinary(dir); err == nil {
		t.Fatal("a binary with no models beside it must not count as an installed engine")
	}
	for _, name := range parakeetModelFiles {
		if err := os.WriteFile(filepath.Join(pkDir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	got, err := parakeetBinary(dir)
	if err != nil {
		t.Fatalf("parakeetBinary: %v", err)
	}
	if got != bin {
		t.Errorf("got %q, want %q", got, bin)
	}
}

// transcribeParakeetWAV reads the segments off stdout and ignores the banner sherpa-onnx writes to stderr on every successful run.
func TestTranscribeParakeetWAV(t *testing.T) {
	bin := fakeParakeet(t, parakeetStdout, "Creating recognizer ...\nElapsed seconds: 1.639 s")
	segs, err := transcribeParakeetWAV(context.Background(), bin, testWAV(t), speakerMe, "", 0)
	if err != nil {
		t.Fatalf("transcribeParakeetWAV: %v", err)
	}
	if len(segs) != 6 {
		t.Fatalf("got %d segments, want 6", len(segs))
	}
}

// Parakeet with VAD skips the silence outright and runs tens of times faster than real time, so whisper's "too fast to be real" guard would call every silent meeting a failure. A silent recording has to come back as no speech, not as an error.
func TestTranscribeParakeetWAV_SilentMeetingIsNotAFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mic.wav")
	w, err := newWAV(path)
	if err != nil {
		t.Fatalf("newWAV: %v", err)
	}
	if _, err := w.Write(make([]byte, sampleRate*2*600)); err != nil { // ten minutes of silence
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	bin := fakeParakeet(t, "", "")
	segs, err := transcribeParakeetWAV(context.Background(), bin, path, speakerMe, "", 0)
	if err != nil {
		t.Fatalf("transcribeParakeetWAV: %v", err)
	}
	if len(segs) != 0 {
		t.Errorf("got %+v, want no segments", segs)
	}
}

// A run that exits non-zero is a failure, and the message has to carry what sherpa-onnx complained about rather than just "exit status 1".
func TestTranscribeParakeetWAV_ReportsAFailedRun(t *testing.T) {
	dir := t.TempDir()
	for _, name := range parakeetModelFiles {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	bin := filepath.Join(dir, parakeetBinaryName)
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'Invalid tokens file' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := transcribeParakeetWAV(context.Background(), bin, testWAV(t), speakerMe, "", 0)
	if err == nil {
		t.Fatal("a non-zero exit must be an error")
	}
	if !strings.Contains(err.Error(), "Invalid tokens file") {
		t.Errorf("error %q does not carry what sherpa-onnx complained about", err)
	}
}

// A whisper.cpp build needs two things a whisperfile does not: the model to load, which a whisperfile has baked into itself, and which GPU to run it on. The model is found beside the binary.
func TestWhisperCPPArgs(t *testing.T) {
	t.Setenv("ORA_DATA_DIR", t.TempDir())
	dir := t.TempDir()
	bin := filepath.Join(dir, whisperCPPBinaryName)
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	// With no model beside it the binary is a whisperfile, which takes neither flag.
	if got := whisperCPPArgs(bin); got != nil {
		t.Errorf("whisperCPPArgs = %v, want nil for a binary with no model beside it", got)
	}
	model := filepath.Join(dir, whisperCPPModelName)
	if err := os.WriteFile(model, []byte("lmgg"), 0o644); err != nil {
		t.Fatalf("write model: %v", err)
	}
	got := whisperCPPArgs(bin)
	if len(got) < 2 || got[0] != "-m" || got[1] != model {
		t.Errorf("whisperCPPArgs = %v, want it to load %s", got, model)
	}
}

// whisperCPPBinary finds the whisper.cpp build next to its model under dataDir/whispercpp, and refuses a half-finished install rather than failing deep inside whisper.cpp.
func TestWhisperCPPBinary(t *testing.T) {
	dir := t.TempDir()
	if _, err := whisperCPPBinary(dir); err == nil {
		t.Fatal("an empty data directory must not yield a whisper.cpp binary")
	}
	wcDir := filepath.Join(dir, "whispercpp")
	if err := os.MkdirAll(wcDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	bin := filepath.Join(wcDir, whisperCPPBinaryName)
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := whisperCPPBinary(dir); err == nil {
		t.Fatal("a binary with no model beside it must not count as an installed engine")
	}
	if err := os.WriteFile(filepath.Join(wcDir, whisperCPPModelName), []byte("lmgg"), 0o644); err != nil {
		t.Fatalf("write model: %v", err)
	}
	got, err := whisperCPPBinary(dir)
	if err != nil {
		t.Fatalf("whisperCPPBinary: %v", err)
	}
	if got != bin {
		t.Errorf("got %q, want %q", got, bin)
	}
}

// whisper-medium on the GPU takes about 2.2 GB of this machine's 4 GB card, so the two streams of a call cannot be decoded at the same time — the second run would fail to allocate. A whisper.cpp run therefore has to wait for any other whisper.cpp run to finish, even though transcriptFor starts both at once.
// A whisperfile is not affected: it decodes on the CPU, where running both at once is the whole point.
func TestTranscribeWAV_SerialisesWhisperCPPRuns(t *testing.T) {
	t.Setenv("ORA_DATA_DIR", t.TempDir())
	dir := t.TempDir()
	bin := filepath.Join(dir, whisperCPPBinaryName)
	// The script records that it is running, holds for a moment, and clears the marker, so an overlap leaves the marker behind for the other run to find.
	body := "#!/bin/sh\nif [ -e " + dir + "/running ]; then echo OVERLAP >&2; fi\ntouch " + dir + "/running\nsleep 0.3\nrm -f " + dir + "/running\nexit 0\n"
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, whisperCPPModelName), []byte("lmgg"), 0o644); err != nil {
		t.Fatalf("write model: %v", err)
	}

	wav := testWAV(t)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = transcribeWAV(context.Background(), bin, wav, speakerMe, "", 0)
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil && strings.Contains(err.Error(), "OVERLAP") {
			t.Fatal("two whisper.cpp runs overlapped, which would exhaust the GPU's memory")
		}
	}
}
