//go:build linux

package audio

import (
	"math"
	"sync/atomic"
)

const (
	// echoRatio is how far below the playback level a mic chunk must sit to count as June's own voice coming back. 0.25 is -12 dB: through PipeWire's webrtc canceller the leftover echo measured 22-26 dB below the playback level at the 99th percentile over three recordings, so this leaves about 10 dB of room.
	echoRatio = 0.25

	// echoRefDecay is the share of the playback level still counted 20 ms after playback drops, so room reverb and the canceller's tail stay covered. 0.85 per 20 ms falls 60 dB in 0.8 s, the rate the macOS EchoGuard uses.
	echoRefDecay = 0.85

	// echoDecaySamples is the 20 ms at 24 kHz that echoRefDecay is quoted for.
	echoDecaySamples = 480

	// micOpenMuteSamples is 1.0 s of 24 kHz audio. In 4 of 7 recordings the first 0.5 s of june_ec_source after the stream opened carried unsuppressed noise up to -37 dBFS, which the Live server heard as speech and used to cut the greeting.
	micOpenMuteSamples = 24000
)

// playbackPeak holds the loudest RMS, as float64 bits in int16 sample units, that the speaker has written to the audio server since the echo guard last asked. The speaker raises it and takePlaybackPeak resets it.
var playbackPeak atomic.Uint64

// notePlayback records the RMS of audio the speaker is about to play. Input: that RMS in int16 sample units. Output: none.
func notePlayback(rms float64) {
	for {
		old := playbackPeak.Load()
		if rms <= math.Float64frombits(old) || playbackPeak.CompareAndSwap(old, math.Float64bits(rms)) {
			return
		}
	}
}

// takePlaybackPeak returns the loudest playback RMS since the previous call and starts the next window.
func takePlaybackPeak() float64 {
	return math.Float64frombits(playbackPeak.Swap(0))
}

// sampleRMS is the root mean square of s16 samples, in sample units (0 to 32768). Input: the samples. Output: the RMS, 0 for no samples.
func sampleRMS(samples []int16) float64 {
	if len(samples) == 0 {
		return 0
	}
	var sum float64
	for _, s := range samples {
		f := float64(s)
		sum += f * f
	}
	return math.Sqrt(sum / float64(len(samples)))
}

// echoGuard silences the mic chunks that are only June's own voice returning, and everything in the first second after the stream opened, before the audio leaves the capture path. One guard per capture stream, used from the stream's single callback.
type echoGuard struct {
	muteLeft int
	ref      float64
}

func newEchoGuard() *echoGuard {
	return &echoGuard{muteLeft: micOpenMuteSamples}
}

// process zeroes samples in place when they fall in the open-time mute or are quieter than echoRatio times the playback reference. Input: one chunk of 24 kHz s16 mic samples and the loudest playback RMS since the previous chunk. Output: true when the chunk was silenced.
func (g *echoGuard) process(samples []int16, playback float64) bool {
	g.ref = math.Max(playback, g.ref*math.Pow(echoRefDecay, float64(len(samples))/echoDecaySamples))
	if g.muteLeft > 0 {
		g.muteLeft -= len(samples)
		clear(samples)
		return true
	}
	if sampleRMS(samples) <= echoRatio*g.ref {
		clear(samples)
		return true
	}
	return false
}
