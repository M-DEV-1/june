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

// WhisperCPPBinary finds the whisper.cpp build next to its model under dataDir/whispercpp, and refuses a half-finished install rather than failing deep inside whisper.cpp.
func TestWhisperCPPBinary(t *testing.T) {
	dir := t.TempDir()
	if _, err := WhisperCPPBinary(dir); err == nil {
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
	if _, err := WhisperCPPBinary(dir); err == nil {
		t.Fatal("a binary with no model beside it must not count as an installed engine")
	}
	if err := os.WriteFile(filepath.Join(wcDir, whisperCPPModelName), []byte("lmgg"), 0o644); err != nil {
		t.Fatalf("write model: %v", err)
	}
	got, err := WhisperCPPBinary(dir)
	if err != nil {
		t.Fatalf("WhisperCPPBinary: %v", err)
	}
	if got != bin {
		t.Errorf("got %q, want %q", got, bin)
	}
}

// whisper-medium on the GPU takes about 2.2 GB of this machine's 4 GB card, so the two streams of a call cannot be decoded at the same time — the second run would fail to allocate. A whisper.cpp run therefore has to wait for any other whisper.cpp run to finish, even though transcriptFor starts both at once.
func TestTranscribeWAV_SerialisesWhisperCPPRuns(t *testing.T) {
	t.Setenv("JUNE_DATA_DIR", t.TempDir())
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

// A GPU decode waits for the embedding server to yield the card instead of falling back to the CPU: the releaser is retried until it reports the server is down, and only then does whisper run.
func TestTranscribeWAV_WaitsForTheGPUUntilTheEmbedderYields(t *testing.T) {
	t.Setenv("JUNE_DATA_DIR", t.TempDir())
	dir := t.TempDir()
	bin := filepath.Join(dir, whisperCPPBinaryName)
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, whisperCPPModelName), []byte("lmgg"), 0o644); err != nil {
		t.Fatalf("write model: %v", err)
	}

	old, oldWait := gpuReleaser, gpuWaitInterval
	defer func() { gpuReleaser, gpuWaitInterval = old, oldWait }()
	gpuWaitInterval = time.Millisecond
	calls := 0
	gpuReleaser = func() bool { calls++; return calls >= 3 }

	if _, err := transcribeWAV(context.Background(), bin, testWAV(t), speakerMe, "", 0); err != nil {
		t.Fatalf("transcribeWAV: %v", err)
	}
	if calls < 3 {
		t.Errorf("whisper ran after %d release attempts, want it to keep waiting until the embedder yields", calls)
	}
}

// The wait honours cancellation: a shutdown must not leave a transcription loop spinning against a pinned embedder.
func TestTranscribeWAV_GPUWaitStopsOnContextCancel(t *testing.T) {
	t.Setenv("JUNE_DATA_DIR", t.TempDir())
	dir := t.TempDir()
	bin := filepath.Join(dir, whisperCPPBinaryName)
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, whisperCPPModelName), []byte("lmgg"), 0o644); err != nil {
		t.Fatalf("write model: %v", err)
	}

	old, oldWait := gpuReleaser, gpuWaitInterval
	defer func() { gpuReleaser, gpuWaitInterval = old, oldWait }()
	gpuWaitInterval = time.Millisecond
	gpuReleaser = func() bool { return false }

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := transcribeWAV(ctx, bin, testWAV(t), speakerMe, "", 0); err == nil {
		t.Fatal("a cancelled wait must surface an error, not hang or run anyway")
	}
}

// Five of eleven dictations in the six days to 2026-09-13 died for want of card memory and were redone on the CPU, costing 12.9 to 20.7 seconds each while the user waited. The releaser that asks the embedding server off the card existed the whole time and only the meeting path called it, so a dictation walked onto a full card, crashed, and paid for the retry. Asking is the first thing a run does now, whoever started it.
func TestRunWhisper_AsksTheCardsOtherTenantToLeaveFirst(t *testing.T) {
	t.Setenv("JUNE_DATA_DIR", t.TempDir())
	dir := t.TempDir()
	bin := filepath.Join(dir, "stub")
	// The stub records that it ran, so the order of the ask and the run can be checked rather than assumed.
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ntouch "+filepath.Join(dir, "ran")+"\necho words\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	var askedBeforeTheRun bool
	var asked int
	SetGPUReleaser(func() bool {
		asked++
		_, err := os.Stat(filepath.Join(dir, "ran"))
		askedBeforeTheRun = os.IsNotExist(err)
		return true
	})
	t.Cleanup(func() { SetGPUReleaser(nil) })

	if _, _, err := RunWhisper(context.Background(), bin, nil); err != nil {
		t.Fatalf("RunWhisper: %v", err)
	}
	if asked == 0 {
		t.Fatal("the run never asked the card's other tenant to leave, so a dictation still walks onto a full card")
	}
	if !askedBeforeTheRun {
		t.Errorf("asked after the decode had already started, which is too late to stop the crash")
	}
}

// Voice activity detection is what keeps a meeting the user barely spoke in from coming back empty: handed a whole stream, whisper decodes the near-silence around a short turn as non-speech and never comes out of that state. In the 16 September 2026 standup that lost the user's entire update — 51 seconds of speech inside 23 minutes of quiet — and the minutes said he had not spoken. The flags go on only when the VAD model is installed beside the whisper model, so a build without it transcribes exactly as before.
func TestWhisperCPPArgs_TurnsOnVADWhenItsModelIsInstalled(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, whisperCPPBinaryName)
	if err := os.WriteFile(filepath.Join(dir, whisperCPPModelName), []byte("lmgg"), 0o644); err != nil {
		t.Fatalf("write model: %v", err)
	}
	if got := strings.Join(whisperCPPArgs(bin), " "); strings.Contains(got, "--vad") {
		t.Errorf("whisperCPPArgs = %v, want no VAD flags when its model is not installed", got)
	}

	vad := filepath.Join(dir, whisperVADModelName)
	if err := os.WriteFile(vad, []byte("lmgg"), 0o644); err != nil {
		t.Fatalf("write vad model: %v", err)
	}
	got := strings.Join(whisperCPPArgs(bin), " ")
	if !strings.Contains(got, "--vad") || !strings.Contains(got, "-vm "+vad) {
		t.Errorf("whisperCPPArgs = %v, want it to enable VAD with %s", got, vad)
	}
}

// whisper-cli's own default is -l en, which does not mean "prefer English": told the audio is English, it prints [NON-ENGLISH SPEECH] for everything that is not, and those lines are dropped as non-speech markers. A 10-minute slice of the 16 September 2026 evening call came back as 456 such markers out of 461 lines, so the whole meeting reached the minutes as an empty transcript and the write-up said nobody had spoken. Auto-detection transcribes the same slice properly; --translate makes it worse, not better.
func TestWhisperCPPArgs_LetsWhisperDetectTheLanguage(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, whisperCPPBinaryName)
	if err := os.WriteFile(filepath.Join(dir, whisperCPPModelName), []byte("lmgg"), 0o644); err != nil {
		t.Fatalf("write model: %v", err)
	}
	if got := strings.Join(whisperCPPArgs(bin), " "); !strings.Contains(got, "-l auto") {
		t.Errorf("whisperCPPArgs = %v, want it to auto-detect the spoken language", got)
	}
}
