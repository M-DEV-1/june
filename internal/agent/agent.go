package agent

import (
	"context"
	"ora/internal/audio"
)

// read-only interface to sqlite db
type ContextReader interface {
	GetImplicitContext(ctx context.Context) ([]string, error)
}

type Agent struct {
	mic              audio.Microphone
	speaker          audio.Speaker
	brain            ContextReader
	apiKey           string
	TextChan         chan string // this is for tui text input
	TextResponseChan chan string // results for tui text resp
}

// initializer and orchestrates all hardware (2) and memory (1) moduels
func NewAgent(mic audio.Microphone, speaker audio.Speaker, brain ContextReader, apiKey string) *Agent {
	return &Agent{
		mic:              mic,
		speaker:          speaker,
		brain:            brain,
		apiKey:           apiKey,
		TextChan:         make(chan string, 10),
		TextResponseChan: make(chan string, 10),
	}
}
