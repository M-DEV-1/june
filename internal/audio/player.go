package audio

import (
	"fmt"
	"io"

	"github.com/ebitengine/oto/v3"
)

// number of hours spent here: 2

type otoPlayer struct {
	ctx *oto.Context
	// oto is a low-level os-agnostic audio lib (speaker)
	// we store WCA handles here if later needed for cleanup

	// Windows Core Audio 2006, lowest audio level possible, allows contains a share mode for multi-active-window mic capturing
	player     *oto.Player
	pipeWriter *io.PipeWriter
	// for continuous streaming
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

	pr, pw := io.Pipe() // pipereader, pipewriter

	// without priming, or init, this will hang indefinitely because io.Pipe is synchronous and oto will wait for someone to call Write()

	ctx, readyChan, err := oto.NewContext(op)
	if err != nil {
		return nil, fmt.Errorf("failed to init oto context: %w", err)
	}
	<-readyChan

	// usually lazy inits are better for this but using channels is better because real time audio can't wait for init (priming)
	go func() {
		pw.Write(make([]byte, 4096)) // 4kb silence
	}()

	player := ctx.NewPlayer(pr) // greedily trying to read first buffer of audio from pipe
	// oto will look for data here, and eat the instantiated silence, and return immediately
	player.Play()

	return &otoPlayer{
		ctx:        ctx,
		player:     player,
		pipeWriter: pw,
	}, nil
}

func (p *otoPlayer) Play(pcm []byte) error {
	go func() {
		p.pipeWriter.Write(pcm)
	}()
	// this blocks naturally as log as needed to make room in the pipe
	// offloaded to a bg thread, because without it caused frezing agent loop
	return nil
}

func (p *otoPlayer) Close() error {
	// no specific context cleanup required, just to satisfy og struct
	// idk, while oto doesn't require the cleanup anymore, I think it's still beneficial to maintain this pattern, just in case I change stuff later (i did need it apparently)

	p.pipeWriter.Close() // EOF signal to oto.Player
	return nil
}
