package recorder

import (
	"encoding/binary"
	"os"
)

// sampleRate is what whisper wants and all whisper does with anything else is resample it, so capture at 16 kHz mono s16le and skip the conversion.
const sampleRate = 16000

// wavHeaderSize is the canonical RIFF/fmt/data header: 12 bytes RIFF + 24 bytes fmt + 8 bytes data. Nothing else goes in it — whisper's WAV reader (miniaudio) rejects a file that has a LIST/INFO chunk sitting between fmt and data, which is what ffmpeg writes by default.
const wavHeaderSize = 44

// wavWriter streams raw 16 kHz mono s16le samples into a WAV file. The header goes down first with zeroed sizes and is patched on Close, so a process killed mid-recording still leaves every captured sample on disk for repairWAV to reclaim.
type wavWriter struct {
	f *os.File
	n int64
}

func newWAV(path string) (*wavWriter, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(wavHeader(0)); err != nil {
		f.Close()
		return nil, err
	}
	return &wavWriter{f: f}, nil
}

func (w *wavWriter) Write(p []byte) (int, error) {
	n, err := w.f.Write(p)
	w.n += int64(n)
	return n, err
}

// Close patches the two size fields to match what was written and closes the file.
func (w *wavWriter) Close() error {
	if err := patchSizes(w.f, w.n); err != nil {
		w.f.Close()
		return err
	}
	return w.f.Close()
}

// repairWAV recomputes a file's size fields from its actual length. Running it on a cleanly closed file writes back the values already there, so the transcription path can call it unconditionally and get crash recovery for free.
func repairWAV(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	data := info.Size() - wavHeaderSize
	if data < 0 {
		data = 0
	}
	return patchSizes(f, data)
}

// whisperFrameChunk is the block size whisper's miniaudio decoder reads in. A file whose frame count is an exact multiple of it fails to decode: the decoder asks for every frame at once, the final read comes back "At end", and whisper reports it could not read the file. Declaring one frame fewer sidesteps it at a cost of 1/16000 of a second.
const whisperFrameChunk = 512

// patchSizes writes the RIFF chunk size and the data chunk size for dataLen bytes of samples, rounded down off a frame count whisper cannot read.
func patchSizes(f *os.File, dataLen int64) error {
	if frames := dataLen / 2; frames > 0 && frames%whisperFrameChunk == 0 {
		dataLen -= 2
	}
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], uint32(36+dataLen))
	if _, err := f.WriteAt(buf[:], 4); err != nil {
		return err
	}
	binary.LittleEndian.PutUint32(buf[:], uint32(dataLen))
	_, err := f.WriteAt(buf[:], 40)
	return err
}

// wavHeader builds the 44-byte header for dataLen bytes of 16 kHz mono s16le samples.
func wavHeader(dataLen uint32) []byte {
	h := make([]byte, wavHeaderSize)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], 36+dataLen)
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)         // fmt chunk size
	binary.LittleEndian.PutUint16(h[20:], 1)          // PCM
	binary.LittleEndian.PutUint16(h[22:], 1)          // mono
	binary.LittleEndian.PutUint32(h[24:], sampleRate) // sample rate
	binary.LittleEndian.PutUint32(h[28:], sampleRate*2)
	binary.LittleEndian.PutUint16(h[32:], 2)  // block align: 1 channel * 2 bytes
	binary.LittleEndian.PutUint16(h[34:], 16) // bits per sample
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], dataLen)
	return h
}
