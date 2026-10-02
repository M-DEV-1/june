package audio

import (
	"encoding/binary"
	"math"
)

// The helpers here turn what a Windows audio device hands over into the 16-bit mono stream the rest of June reads. They carry no build tag so their tests run on Linux.

// toMono averages interleaved frames into one channel on the int16 scale.
// Input: raw little-endian samples, the channel count, and whether they are float32 (else int16). Output: one value per frame.
func toMono(data []byte, channels int, float bool) []float64 {
	size := 2
	if float {
		size = 4
	}
	frames := len(data) / (size * channels)
	out := make([]float64, frames)
	for f := range out {
		var sum float64
		for c := 0; c < channels; c++ {
			b := data[(f*channels+c)*size:]
			if float {
				sum += float64(math.Float32frombits(binary.LittleEndian.Uint32(b))) * 32768
			} else {
				sum += float64(int16(binary.LittleEndian.Uint16(b)))
			}
		}
		out[f] = sum / float64(channels)
	}
	return out
}

// resampler converts a mono stream from one rate to another by linear interpolation, carrying its position across calls so that packet boundaries add no drift.
// ponytail: linear interpolation without a low-pass filter aliases above the target Nyquist, fine for speech into whisper; use a windowed-sinc filter if music or high-frequency detail matters.
type resampler struct {
	from, to int
	// pos is where the next output sample falls, in input samples relative to the start of the next push; -1 <= pos < 0 means it lies between prev and that push's first sample.
	pos  float64
	prev float64
}

// push resamples one packet. Input: samples at r.from. Output: samples at r.to.
func (r *resampler) push(in []float64) []float64 {
	if len(in) == 0 {
		return nil
	}
	step := float64(r.from) / float64(r.to)
	at := func(i int) float64 {
		if i < 0 {
			return r.prev
		}
		return in[i]
	}
	var out []float64
	for ; int(math.Floor(r.pos))+1 < len(in); r.pos += step {
		i := int(math.Floor(r.pos))
		a := at(i)
		out = append(out, a+(r.pos-float64(i))*(at(i+1)-a))
	}
	r.pos -= float64(len(in))
	r.prev = in[len(in)-1]
	return out
}

// pcm16 encodes samples on the int16 scale as 16-bit little-endian PCM, clamping each to the int16 range.
func pcm16(samples []float64) []byte {
	buf := make([]byte, len(samples)*2)
	for i, v := range samples {
		v = math.Max(-32768, math.Min(32767, math.Round(v)))
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(int16(v)))
	}
	return buf
}

// gapFrames returns how many frames of silence belong before a packet so a stream that stops delivering while nothing plays (WASAPI loopback) stays on the wall clock.
// Input: the packet's timestamp and the stream's start, both in 100 ns units, the stream's rate, and the frames written so far. Output: the frames of silence to write first, 0 when the gap is under 50 ms of jitter or the stream is ahead.
func gapFrames(posHNS, startHNS int64, rate int, written int64) int64 {
	gap := (posHNS-startHNS)*int64(rate)/10_000_000 - written
	if gap < int64(rate)/20 {
		return 0
	}
	return gap
}

// level is the loudness of a chunk of 16-bit little-endian PCM for the waveform: RMS scaled by three, since speech RMS sits about three times below its peak, and capped at 1.
func level(pcm []byte) float64 {
	n := len(pcm) / 2
	if n == 0 {
		return 0
	}
	var sum float64
	for i := 0; i < n; i++ {
		f := float64(int16(binary.LittleEndian.Uint16(pcm[i*2:])))
		sum += f * f
	}
	return math.Min(1, math.Sqrt(sum/float64(n))/32768*3)
}

// converter turns one device packet into 16-bit mono PCM at the stream's rate.
type converter struct {
	// block is the size of one source frame in bytes.
	block int
	// direct is set when the device already delivers 16-bit mono at the wanted rate, and the packet only needs copying out of device memory.
	direct   bool
	channels int
	float    bool
	rs       resampler
}

// convert returns raw as 16-bit little-endian mono PCM at the target rate. Input: whole source frames. Output: a fresh slice the caller may keep.
func (c *converter) convert(raw []byte) []byte {
	if c.direct {
		return append([]byte(nil), raw...)
	}
	return pcm16(c.rs.push(toMono(raw, c.channels, c.float)))
}
