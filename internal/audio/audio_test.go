package audio_test

import (
	"context"
	"june/internal/audio"
	"testing"
	"time"
)

// Needs a real input device: the mic must deliver PCM on the channel and the channel must close when the context is cancelled.
func TestMicrophone_Capture(t *testing.T) {
	if testing.Short() {
		t.Skip("needs a real input device; CI runs -short")
	}
	mic, err := audio.NewMic()
	if err != nil {
		t.Fatalf("Failed to init mic: %+v", err)
	}
	defer mic.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	micChan, err := mic.StartCapture(ctx)
	if err != nil {
		t.Fatalf("Failed to start capture: %+v", err)
	}

	var bytesCaptured int
	for pcm := range micChan {
		bytesCaptured += len(pcm)
	}
	if bytesCaptured == 0 {
		t.Error("Expected to capture audio bytes, got 0")
	}
}
