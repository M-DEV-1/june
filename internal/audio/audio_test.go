package audio_test

import (
	"context"
	"ora/internal/audio"
	"testing"
	"time"
)

func TestMicrophone_Capture(t *testing.T) {
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
	for {
		select {
		case pcm := <-micChan:
			bytesCaptured += len(pcm)
		case <-ctx.Done():
			goto Verify
		}
	}

Verify:
	if bytesCaptured == 0 {
		t.Error("Expected to capture audio bytes, got 0")
	} else {
		t.Logf("Microphone captured %d bytes", bytesCaptured)
	}
}

// FLAKY TEST
func TestSpeaker_Playing(t *testing.T) {
	speaker, err := audio.NewSpeaker()
	if err != nil {
		t.Fatalf("Failed to init speaker: %+v", err)
	}
	defer speaker.Close()

	// TODO: find ways to improve this test by actually playing smth maybe

	// simulating dummy audio streaming
	dummyChunk := make([]byte, 1024) // 1kb of silence

	// 3 chunks of silence
	for i := 0; i < 3; i++ {
		err := speaker.Play(dummyChunk)
		if err != nil {
			t.Fatalf("Failed to play chunk %d: %+v", i, err)
		}
	}

	t.Log("Speaker successfully processed streaming chunks")
}
