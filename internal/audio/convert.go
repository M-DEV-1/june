package audio

import (
	"encoding/binary"
	"log/slog"
	"math"
	"time"
)

// The helpers here turn what a Windows audio device hands over into the 16-bit mono stream the rest of June reads; the resampler also serves the speaker, which converts the other way (outConverter, player_windows.go). They carry no build tag so their tests run on Linux.

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

// hns is one second in the 100 ns units WASAPI and the performance counter count in.
const hns = 10_000_000

// clockJitter is how far a packet may land from where the previous one ended, 50 ms, before the difference counts as a gap or as a bad stamp.
const clockJitter = hns / 20

// packetClock keeps a WASAPI stream on the wall clock. WASAPI stamps each packet with the performance-counter time of its first frame and delivers nothing while a loopback device plays nothing or while a lost device is being reopened, so the silence a stream skipped is written back in before the packet that ends it.
// Gaps are measured from where the previous packet ended, not from the start of the stream: a device clock runs about 100 ppm off the performance counter, and measured from the start that drift passes the 50 ms jitter allowance every few minutes and puts silence into the middle of speech.
type packetClock struct {
	rate int   // the output rate, in frames per second
	next int64 // where the next packet should start, in 100 ns performance-counter units
	// lead is how far the performance counter has run ahead of the unbiased interrupt time, which stops while the machine sleeps, as the read loop last measured it (see sleepLead); cut is what lead was when sleep was last taken out of a gap. lead-cut is sleep this stream has not written a gap for yet. Both stay 0 where nothing measures them, and no gap is then taken for sleep.
	lead, cut int64
}

// silenceBefore returns how many output frames of silence belong before one packet, and moves the clock past that packet.
// Input: the packet's performance-counter stamp, its length in source frames at srcRate, whether the device flagged the stamp as invalid (AUDCLNT_BUFFERFLAGS_TIMESTAMP_ERROR), and the performance counter now; times are in 100 ns units. Output: the frames of silence to write first; 0 when the packet follows on within clockJitter, and 0 for a stamp that is flagged, later than now, or earlier than the previous packet's end by more than clockJitter, in which case the packet is taken to follow on directly.
// A trusted stamp is never later than now, so the silence written is never longer than the time that really passed.
func (c *packetClock) silenceBefore(stamp int64, frames, srcRate int, stampBad bool, now int64) int64 {
	length := int64(frames) * hns / int64(srcRate)
	if stampBad || stamp > now || stamp < c.next-clockJitter {
		c.next += length
		return 0
	}
	gap := stamp - c.next
	c.next = stamp + length
	if gap < clockJitter {
		return 0
	}
	return c.framesFor(gap)
}

// sleepBreak is the silence a recording gets in place of the time the machine slept, one second: enough that whisper does not run the last words before the sleep into the first ones after it, and the same on both streams of a meeting, so the two stay lined up with each other. The performance counter keeps counting through standby and hibernation, so a recording left running over a sleep has a gap as long as the sleep; written as silence, a night's sleep in the middle of a call was some 32 KB a second per stream, hours of nothing for whisper to decode, and over a weekend more than a WAV's 4 GB can hold.
const sleepBreak = hns

// minSleep is the shortest measured sleep taken out of a gap, one second. The unbiased interrupt time moves only on a clock interrupt, up to 15.6 ms apart, so lead wobbles by that much with no sleep at all.
const minSleep = hns

// maxAwakeGap is the longest stretch the machine was measured awake that a recording still writes back as silence, five minutes. A recording's real gaps are a loopback's quiet between two polls and a lost device's reopen, a few seconds at most, so a longer one is a suspension the unbiased interrupt time did not see, such as an app frozen in modern standby, and is cut to sleepBreak like a sleep.
const maxAwakeGap = 5 * 60 * hns

// framesFor turns a gap the clock has already moved past into the frames of silence to write for it. The part of it the machine slept through is measured (lead-cut) rather than guessed from the gap's length, and becomes one sleepBreak; the rest, time the machine was awake and the device gave nothing, is written in full. Measured, a headset that takes seconds to reopen after the machine wakes still gets those seconds, while the other stream of the meeting records through them, so the two stay in step.
// Input: the gap, in 100 ns units. Output: the frames to write.
func (c *packetClock) framesFor(gap int64) int64 {
	awake, broken := gap, false
	if asleep := min(gap, c.lead-c.cut); asleep >= minSleep {
		c.cut += asleep
		awake -= asleep
		broken = true
		slog.Info("the machine slept during a recording; writing a short break instead of the sleep as silence", "slept", time.Duration(asleep*100))
	}
	if awake >= maxAwakeGap {
		slog.Warn("a recording's device gave nothing for longer than an awake device does; writing a short break instead", "gap", time.Duration(awake*100))
		awake, broken = 0, true
	}
	if broken {
		awake += sleepBreak
	}
	return awake * int64(c.rate) / hns
}

// silenceLag is how far behind now a recording writes the silence of a device that delivers nothing, half a second. A packet reaches the read loop up to a poll and an engine period after the stamp it carries, so silence written right up to now would take the place that packet belongs in.
const silenceLag = hns / 2

// idle returns how many frames of silence bring the clock up to silenceLag before now, for a device that has delivered nothing while time went on (a loopback while nothing plays), and moves the clock past them. Without it a recording of a device that never plays anything stays empty, and one that goes quiet gets its silence only once the next sound arrives, if one ever does.
// Input: the performance counter now, in 100 ns units, read after a poll that found the device's buffer empty. Output: the frames to write; 0 while the clock is within silenceLag of now.
func (c *packetClock) idle(now int64) int64 {
	behind := now - silenceLag - c.next
	if behind <= 0 {
		return 0
	}
	if c.lead-c.cut >= minSleep || behind >= maxAwakeGap {
		c.next = now - silenceLag
		return c.framesFor(behind)
	}
	frames := behind * int64(c.rate) / hns
	c.next += frames * hns / int64(c.rate)
	return frames
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
	// from is the rate of the frames the device hands over.
	from int
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
