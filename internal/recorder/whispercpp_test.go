package recorder

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
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
	// The install doctor names for Windows is whisper-cli.exe, so that is the file that has to be found there.
	bin := filepath.Join(wcDir, whisperCPPBinaryName)
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
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
	bin := fakeWhisperBin(t, true)
	// Each run marks the directory busy for a moment and fails with OVERLAP when it finds the other run's mark still there.
	t.Setenv("FAKE_WHISPER_BUSY", t.TempDir())

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
	bin := fakeWhisperBin(t, true)

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

// Five of eleven dictations in the six days to 2026-09-13 died for want of card memory and were redone on the CPU, costing 12.9 to 20.7 seconds each while the user waited. The releaser that asks the embedding server off the card existed the whole time and only the meeting path called it, so a dictation walked onto a full card, crashed, and paid for the retry. Asking is the first thing a run does now, whoever started it.
func TestRunWhisper_AsksTheCardsOtherTenantToLeaveFirst(t *testing.T) {
	t.Setenv("JUNE_DATA_DIR", t.TempDir())
	bin := fakeWhisperBin(t, false)
	// The fake records that it ran, so the order of the ask and the run can be checked rather than assumed.
	ran := filepath.Join(t.TempDir(), "ran")
	t.Setenv("FAKE_WHISPER_RAN", ran)

	var askedBeforeTheRun bool
	var asked int
	SetGPUReleaser(func() bool {
		asked++
		_, err := os.Stat(ran)
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
