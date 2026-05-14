package audio

import (
	"context"
	"io"
)

type Microphone interface {
	io.Closer
	// this is what i expect the audio behavior to be
	// open mic and return a channel of 16-bit PCM chunks
	// pulse code modulation is the raw format for sound, uncompressed + zero lag
	// 16-bit is the bit depth/quality, chunks are 20ms to 100ms long
	// audio is 16-bit, every 2 bytes represents 1 single sample of sound
	// 640 bytes, contains 320 moments of sound (like sock pairs)
	StartCapture(ctx context.Context) (<-chan []byte, error)
}

type Speaker interface {
	io.Closer
	// play audio output through speaker
	Play(pcm []byte) error
	// clear the current audio buffer immediately (for interrupts)
	Flush()
}
