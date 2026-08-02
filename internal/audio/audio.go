package audio

import (
	"context"
	"io"
)

type Microphone interface {
	io.Closer
	// opens the mic, returns a channel of 16-bit PCM chunks (raw uncompressed audio, 2 bytes per sample, ~20-100ms per chunk)
	StartCapture(ctx context.Context) (<-chan []byte, error)
	// this is for tui waveform
	CurrentAmplitude() float64
}

type Speaker interface {
	io.Closer
	// play audio output through speaker
	Play(pcm []byte) error
	// clear the current audio buffer immediately (for interrupts)
	Flush()
	// returns peak amp
	CurrentAmplitude() float64
}
