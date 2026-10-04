package audio

import (
	"context"
	"io"
)

type Microphone interface {
	io.Closer
	// opens the mic, returns a channel of 16-bit PCM chunks (raw uncompressed audio, 2 bytes per sample, ~20-100ms per chunk)
	StartCapture(ctx context.Context) (<-chan []byte, error)
	// this is for the window's waveform
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

// Drainer is a Speaker that can wait for what it has been given to finish playing. Play only queues, and Close and Flush both drop what is queued, so a caller that plays one line and closes has to wait in between. Both real speakers are Drainers, which each one's file checks at build time; it is kept out of Speaker so the fakes the agent and voice session tests play into do not need a method nothing there calls.
type Drainer interface {
	// Drain blocks until everything queued has been heard, ctx ends, or the speaker cannot play it, and returns nil only in the first case.
	Drain(ctx context.Context) error
}
