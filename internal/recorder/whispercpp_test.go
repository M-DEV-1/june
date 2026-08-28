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

// A whisper.cpp build needs two things a bare stub does not: the model to load, and which GPU to run it on. The model is found beside the binary.
func TestWhisperCPPArgs(t *testing.T) {
	t.Setenv("ORA_DATA_DIR", t.TempDir())
	dir := t.TempDir()
	bin := filepath.Join(dir, whisperCPPBinaryName)
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	// With no model beside it the binary takes neither flag, which is what lets a test stub run bare.
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

// A GPU decode waits for the embedding server to yield the card instead of falling back to the CPU: the releaser is retried until it reports the server is down, and only then does whisper run.
func TestTranscribeWAV_WaitsForTheGPUUntilTheEmbedderYields(t *testing.T) {
	t.Setenv("ORA_DATA_DIR", t.TempDir())
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
	t.Setenv("ORA_DATA_DIR", t.TempDir())
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
