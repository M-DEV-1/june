package audio

import (
	"bytes"
	"testing"
	"time"
)

// Needs a real microphone and playback device: both streams open, the mic delivers about a second of 16 kHz audio, and Stop returns with nothing written after it. The system side only fills when something is playing, so play a sound during the test to see it too.
func TestMeetingCaptureWindows(t *testing.T) {
	if testing.Short() {
		t.Skip("needs a real input device; CI runs -short")
	}
	var mic, sys bytes.Buffer
	c, err := StartMeetingCapture(&mic, &sys)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	c.Stop()
	micLen, sysLen := mic.Len(), sys.Len()
	time.Sleep(100 * time.Millisecond)
	if mic.Len() != micLen || sys.Len() != sysLen {
		t.Error("a stream wrote after Stop returned")
	}
	if c.Dropped() {
		t.Error("a stream reports dropped after a clean run")
	}
	t.Logf("mic %d bytes (%.2fs), system %d bytes (%.2fs)", micLen, float64(micLen)/32000, sysLen, float64(sysLen)/32000)
	if micLen < 16000 {
		t.Errorf("mic delivered %d bytes in a second, want about 32000", micLen)
	}
}
