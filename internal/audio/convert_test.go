package audio

import (
	"encoding/binary"
	"math"
	"testing"
)

// The fallback path for a device that will not convert for us: interleaved stereo float32 or int16 comes down to one channel on the int16 scale.
func TestToMono(t *testing.T) {
	f := make([]byte, 16)
	for i, v := range []float32{0.5, -0.5, 1, 0.5} {
		binary.LittleEndian.PutUint32(f[i*4:], math.Float32bits(v))
	}
	if got := toMono(f, 2, true); len(got) != 2 || got[0] != 0 || got[1] != 0.75*32768 {
		t.Errorf("float stereo to mono = %v, want [0 24576]", got)
	}
	s := make([]byte, 8)
	for i, v := range []int16{100, 300, -1000, -2000} {
		binary.LittleEndian.PutUint16(s[i*2:], uint16(v))
	}
	if got := toMono(s, 2, false); len(got) != 2 || got[0] != 200 || got[1] != -1500 {
		t.Errorf("int16 stereo to mono = %v, want [200 -1500]", got)
	}
}

// A second of audio fed in WASAPI-sized packets must come out as a second at the target rate: a resampler that rounds per packet drifts by up to a sample every 10 ms, which is 0.6% at 44.1 kHz and puts the two meeting tracks visibly apart within minutes.
func TestResamplerKeepsLength(t *testing.T) {
	for _, from := range []int{44100, 48000} {
		r := resampler{from: from, to: 16000}
		total := 0
		ramp := 0.0
		for sent := 0; sent < from; {
			n := min(from/100, from-sent)
			in := make([]float64, n)
			for i := range in {
				in[i] = ramp
				ramp++
			}
			out := r.push(in)
			// Every output sample of a ramp lies on the ramp: sample j sits at input position j*from/to.
			for j, v := range out {
				want := float64(total+j) * float64(from) / 16000
				if math.Abs(v-want) > 1e-6*want+1e-6 {
					t.Fatalf("%d Hz: output %d = %v, want %v", from, total+j, v, want)
				}
			}
			total += len(out)
			sent += n
		}
		if total < 15999 || total > 16000 {
			t.Errorf("%d Hz: one second resampled to %d samples, want 16000", from, total)
		}
	}
}

// Loopback delivers nothing while nothing plays, so the gap before a packet is filled with silence; small timing jitter and packets that arrive early are left alone.
func TestGapFrames(t *testing.T) {
	const second = 10_000_000
	cases := []struct {
		name         string
		pos, written int64
		want         int64
	}{
		{"first sound one second in, nothing written yet", second, 0, 16000},
		{"on time within jitter", second, 15990, 0},
		{"device clock ahead of the counter", second, 16100, 0},
		{"half-second silence mid-call", 3 * second, 40000, 8000},
	}
	for _, c := range cases {
		if got := gapFrames(c.pos, 0, 16000, c.written); got != c.want {
			t.Errorf("%s: gapFrames = %d, want %d", c.name, got, c.want)
		}
	}
}
