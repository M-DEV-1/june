package audio

import (
	"bytes"
	"fmt"
	"time"

	"github.com/ebitengine/oto/v3"
)

type otoPlayer struct {
	ctx *oto.Context
	// oto is a low-level os-agnostic audio lib (speaker)
	// we store WCA handles here if later needed for cleanup

	// Windows Core Audio 2006, lowest audio level possible, allows contains a share mode for multi-active-window mic capturing
}

func NewSpeaker() (Speaker, error) {
	// initialize oto for speaker
	/* llm api infodump
	- gemini live api requires 24kHz Mono 16-bit
	*/
	op := &oto.NewContextOptions{
		SampleRate:   24000,
		ChannelCount: 1,
		Format:       oto.FormatSignedInt16LE,
	}

	ctx, readyChan, err := oto.NewContext(op)
	if err != nil {
		return nil, fmt.Errorf("failed to init oto context: %w", err)
	}
	<-readyChan

	return &otoPlayer{ctx: ctx}, nil
}

func (p *otoPlayer) Play(pcm []byte) error {
	player := p.ctx.NewPlayer(bytes.NewReader(pcm))

	player.Play()
	for player.IsPlaying() {
		time.Sleep(1 * time.Millisecond)
	}

	time.Sleep(50 * time.Millisecond) // small buffer to ensure all bits of audio is flushed out

	// return player.Close() not required anymore apparently
	return nil
}

func (p *otoPlayer) Close() error {
	// no specific context cleanup required, just to satisfy og struct
	// idk, while oto doesn't require the cleanup anymore, I think it's still beneficial to maintain this pattern, just in case I change stuff later
	return nil
}
