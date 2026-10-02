package audio

import "testing"

// Silence goes in only where the device really delivered nothing: not mid-speech from clock drift, not for a stamp the device flagged, and never more than the time that passed for a stamp that is wrong.
func TestPacketClock(t *testing.T) {
	const rate, packet = 16000, 160 // 10 ms packets at 16 kHz
	t.Run("a device clock 100 ppm slow for ten minutes writes no silence", func(t *testing.T) {
		c := packetClock{rate: rate}
		stamp := int64(0)
		for i := 0; i < 60_000; i++ {
			if gap := c.silenceBefore(stamp, packet, rate, false, stamp); gap != 0 {
				t.Fatalf("packet %d: %d frames of silence inserted into continuous sound", i, gap)
			}
			stamp += 100_010 // 10 ms of audio takes 10.001 ms on this device
		}
	})
	t.Run("two seconds with no packets are written back as two seconds of silence", func(t *testing.T) {
		c := packetClock{rate: rate}
		c.silenceBefore(0, packet, rate, false, 0)
		stamp := int64(100_000 + 2*hns)
		if gap := c.silenceBefore(stamp, packet, rate, false, stamp); gap != 2*rate {
			t.Errorf("gap = %d frames, want %d", gap, 2*rate)
		}
	})
	t.Run("a stamp the device flagged as invalid is not trusted", func(t *testing.T) {
		c := packetClock{rate: rate}
		if gap := c.silenceBefore(5*hns, packet, rate, true, 5*hns); gap != 0 {
			t.Errorf("gap = %d frames for a flagged stamp, want 0", gap)
		}
	})
	t.Run("a stamp later than now is not trusted", func(t *testing.T) {
		c := packetClock{rate: rate}
		if gap := c.silenceBefore(1<<60, packet, rate, false, hns); gap != 0 {
			t.Errorf("gap = %d frames for a stamp in the future, want 0", gap)
		}
	})
}
