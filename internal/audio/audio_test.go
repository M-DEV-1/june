package audio_test

import (
	"context"
	"ora/internal/audio"
	"testing"
	"time"
)

func TestAudioEngine_BidiFlow(t *testing.T) {
	// init os-specific internally so we will have seperate _windows, _linux files
	engine, err := audio.NewEngine()
	if err != nil {
		t.Fatalf("Failed to init audio engine: %v", err)
	}
	defer engine.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// start mic capture
	micChan, err := engine.StartCapture(ctx)
	if err != nil {
		t.Fatalf("Failed to start audio capture: %v", err)
	}

	// we need to verify that we receive data then play it back as echo test
	var bytesCaptured int

	// we be waiting for first audio chunk
	select {
	case pcm := <-micChan:
		bytesCaptured += len(pcm)

		// play back the echo
		err = engine.Play(pcm)
		if err != nil {
			t.Errorf("failed to play audio: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("Timed out waiting for microphone data")
	}

	if bytesCaptured == 0 {
		t.Error("Expected to capture audio bytes, got 0")
	} else {
		t.Logf("Successfully captured and echoed %d bytes of PCM audio", bytesCaptured)
	}
}
