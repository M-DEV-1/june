//go:build windows

package audio

import (
	"context"
	"fmt"

	"github.com/go-ole/go-ole"
)

type winMic struct{}

func NewMic() (Microphone, error) {
	// must initialize COM for the entire audio engine
	// go-ole is bridge between go and msft Component Object Model 1993, universal translator of sorts
	// new com thread

	if err := ole.CoInitialize(0); err != nil {
		return nil, fmt.Errorf("failed to init COM: %w", err)
	}
	return &winMic{}, nil
}

func (m *winMic) StartCapture(ctx context.Context) (<-chan []byte, error) {
	// todo
}

func (m *winMic) Close() error {
	ole.CoUninitialize()
	return nil
}

// todo f32 to i16 transcoder
