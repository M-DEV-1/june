package audio

import (
	"context"
	"fmt"

	"github.com/ebitengine/oto/v3"
	"github.com/go-ole/go-ole" 
)

type winEngine struct {
	otoCtx *oto.Context 
	// oto is a low-level os-agnostic audio lib (speaker)
	// we store WCA handles here if later needed for cleanup

	// Windows Core Audio 2006, lowest audio level possible, allows contains a share mode for multi-active-window mic capturing
}

func NewEngine() (Engine, error) {
	// must initialize COM for the entire audio engine
	// go-ole is bridge between go and msft Component Object Model 1993, universal translator of sorts
	// new com thread
	if err := ole.CoInitialize(0); err != nil {
		return nil, fmt.Errorf("failed to init COM: %w", err)
	}

	// initialize oto for speaker
	/* llm api infodump
	- gemini live api requires 24kHz Mono 16-bit
	*/
	op := &oto.NewContextOptions{
		SampleRate:   24000,
		ChannelCount: 1,
		Format:       oto.FormatSignedInt16LE,
	}
	otoCtx, readyChan, err := oto.NewContext(op)
	if err != nil {
		ole.CoUninitialize()
		// on failure, clean up COM
		return nil, fmt.Errorf("failed to init oto context: %w", err)
	}
	<-readyChan

	return &winEngine{
		otoCtx: otoCtx,
	}, nil
}

func (w *winEngine) Play(pcm []byte) error {
	// TODO
	return nil
}

func (w *winEngine) Close() error {
	// TODO
	return nil
}

func (w *winEngine) StartCapture() (ctx context.Context) (<-chan []byte, error) {
	// TODO go based wca capture loop
	return nil, nil
}
