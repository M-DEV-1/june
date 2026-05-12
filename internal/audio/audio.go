package audio

import "context"

type Engine interface {
	// open mic and return a channel of 16-bit PCM chunks
	// pulse code modulation is the raw format for sound, uncompressed + zero lag
	// 16-bit is the bit depth/quality, chunks are 20ms to 100ms long
	// audio is 16-bit, every 2 bytes represents 1 single sample of sound
	// 640 bytes, contains 320 moments of sound (like sock pairs)
	//
	StartCapture(ctx context.Context) (<-chan []byte, error)

	// play audio output through speaker
	Play(pcm []byte) error

	// release all hardware occupied resources
	Close() error
}
