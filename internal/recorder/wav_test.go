package recorder

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// readHeader pulls the four numbers that matter out of a canonical 44-byte WAV header.
func readHeader(t *testing.T, path string) (riffSize, sampleRate, byteRate, dataSize uint32) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(b) < 44 {
		t.Fatalf("%s: file is %d bytes, want at least a 44-byte header", path, len(b))
	}
	if string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" || string(b[12:16]) != "fmt " || string(b[36:40]) != "data" {
		t.Fatalf("%s: not a canonical RIFF/WAVE/fmt/data layout: %q", path, b[:44])
	}
	return binary.LittleEndian.Uint32(b[4:8]),
		binary.LittleEndian.Uint32(b[24:28]),
		binary.LittleEndian.Uint32(b[28:32]),
		binary.LittleEndian.Uint32(b[40:44])
}

// A closed WAV carries the sizes of what was actually written, and no extra chunks: whisper's WAV reader (miniaudio) rejects a file with a LIST/INFO chunk between fmt and data, which is exactly what ffmpeg writes.
func TestWAVWriter_ClosesWithCorrectSizes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mic.wav")
	w, err := newWAV(path)
	if err != nil {
		t.Fatalf("newWAV: %v", err)
	}
	payload := make([]byte, 3200) // 0.1s of 16 kHz mono s16le
	if _, err := w.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	riff, rate, byteRate, data := readHeader(t, path)
	if data != uint32(len(payload)) {
		t.Errorf("data size = %d, want %d", data, len(payload))
	}
	if riff != uint32(36+len(payload)) {
		t.Errorf("riff size = %d, want %d", riff, 36+len(payload))
	}
	if rate != sampleRate {
		t.Errorf("sample rate = %d, want %d", rate, sampleRate)
	}
	if byteRate != sampleRate*2 {
		t.Errorf("byte rate = %d, want %d", byteRate, sampleRate*2)
	}
}

// whisper's miniaudio decoder refuses to read a WAV whose frame count is an exact multiple of 512: its decoder asks for exactly that many frames, the last read comes back "At end", and it prints "failed to read pcm frames from audio file" and exits 0 — which the pipeline then files as a meeting nobody spoke in. Both the writer and the repair path must therefore declare one frame fewer, which costs 1/16000 of a second.
func TestWAV_HeaderNeverEndsOnA512FrameBoundary(t *testing.T) {
	const bad = 512 * 3 * 2 // bytes: a frame count whisper chokes on
	dir := t.TempDir()

	for _, tc := range []struct {
		name  string
		crash bool // close the fd without patching the header, then repair it, as a killed daemon would
	}{
		{name: "closed.wav"},
		{name: "crashed.wav", crash: true},
	} {
		path := filepath.Join(dir, tc.name)
		w, err := newWAV(path)
		if err != nil {
			t.Fatalf("newWAV: %v", err)
		}
		if _, err := w.Write(make([]byte, bad)); err != nil {
			t.Fatalf("write: %v", err)
		}
		if tc.crash {
			if err := w.f.Close(); err != nil {
				t.Fatalf("raw close: %v", err)
			}
			if err := repairWAV(path); err != nil {
				t.Fatalf("repairWAV: %v", err)
			}
		} else if err := w.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}

		riff, _, _, data := readHeader(t, path)
		if data/2%512 == 0 {
			t.Errorf("%s: header declares %d frames, a multiple of 512 that whisper cannot read", tc.name, data/2)
		}
		if data != bad-2 {
			t.Errorf("%s: header declares %d bytes, want %d — exactly one frame is dropped, no more", tc.name, data, bad-2)
		}
		if riff != 36+data {
			t.Errorf("%s: riff size = %d, want %d", tc.name, riff, 36+data)
		}
	}
}

// A recording killed mid-flight leaves zeroed size fields. repairWAV recomputes them from the file length so the audio is still transcribable.
func TestRepairWAV_FixesCrashedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "system.wav")
	w, err := newWAV(path)
	if err != nil {
		t.Fatalf("newWAV: %v", err)
	}
	if _, err := w.Write(make([]byte, 1600)); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Simulate a crash: close the fd without patching the header.
	if err := w.f.Close(); err != nil {
		t.Fatalf("raw close: %v", err)
	}

	if _, _, _, data := readHeader(t, path); data != 0 {
		t.Fatalf("precondition: expected an unpatched header, got data size %d", data)
	}
	if err := repairWAV(path); err != nil {
		t.Fatalf("repairWAV: %v", err)
	}
	riff, _, _, data := readHeader(t, path)
	if data != 1600 {
		t.Errorf("repaired data size = %d, want 1600", data)
	}
	if riff != 36+1600 {
		t.Errorf("repaired riff size = %d, want %d", riff, 36+1600)
	}
}

// repairWAV is idempotent, so it can run unconditionally before transcription without corrupting a cleanly closed file.
func TestRepairWAV_Idempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mic.wav")
	w, err := newWAV(path)
	if err != nil {
		t.Fatalf("newWAV: %v", err)
	}
	if _, err := w.Write(make([]byte, 640)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := repairWAV(path); err != nil {
		t.Fatalf("repairWAV: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(before) != string(after) {
		t.Error("repairWAV changed a cleanly closed file")
	}
}
