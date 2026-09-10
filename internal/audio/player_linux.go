//go:build linux

package audio

import (
	"math"
	"sync"
	"sync/atomic"

	"github.com/jfreymuth/pulse"
	"github.com/jfreymuth/pulse/proto"
)

type pulseSpeaker struct {
	client     *pulse.Client
	stream     *pulse.PlaybackStream
	mu         sync.Mutex
	buffer     []byte
	chunks     chan []byte
	currentAmp atomic.Uint64
	closeOnce  sync.Once
}

func NewSpeaker() (Speaker, error) {
	c, err := pulse.NewClient()
	if err != nil {
		return nil, err
	}

	s := &pulseSpeaker{
		client: c,
		chunks: make(chan []byte, 10000),
	}

	opts := []pulse.PlaybackOption{
		pulse.PlaybackSampleRate(24000),
		pulse.PlaybackChannels(proto.ChannelMap{proto.ChannelMono}),
		pulse.PlaybackLatency(0.05),
	}
	// Played through the echo-cancel sink when there is one, so the canceller on EchoCancelSource knows what Ora said and can take it back out of the mic (see capture_linux.go).
	if sink, err := c.SinkByID(EchoCancelSink); err == nil {
		opts = append(opts, pulse.PlaybackSink(sink))
	}
	stream, err := c.NewPlayback(pulse.Int16Reader(s.readFn), opts...)
	if err != nil {
		c.Close()
		return nil, err
	}
	s.stream = stream
	stream.Start()
	return s, nil
}

// readFn is called by the pulse library to pull samples for playback.
// It drains the internal buffer first, then tries to refill from chunks without blocking — silence fills any gap so the stream stays alive (mirrors audioStreamer.Read).
func (s *pulseSpeaker) readFn(out []int16) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	filled := 0
	for filled < len(out) {
		if len(s.buffer) < 2 {
			select {
			case chunk, ok := <-s.chunks:
				if !ok {
					break
				}
				if len(s.buffer) == 1 {
					s.buffer = append(s.buffer, chunk...)
				} else {
					s.buffer = chunk
				}
			default:
				// no chunk ready — fill remainder with silence
				for i := filled; i < len(out); i++ {
					out[i] = 0
				}
				filled = len(out)
				break
			}
		}
		if len(s.buffer) < 2 {
			break
		}
		// consume two bytes per sample (s16le)
		for filled < len(out) && len(s.buffer) >= 2 {
			out[filled] = int16(s.buffer[0]) | int16(s.buffer[1])<<8
			s.buffer = s.buffer[2:]
			filled++
		}
	}

	if filled > 0 {
		s.storeAmp(out[:filled])
	}
	return len(out), nil
}

func (s *pulseSpeaker) storeAmp(samples []int16) {
	if len(samples) == 0 {
		return
	}
	var sum float64
	for _, v := range samples {
		f := float64(v)
		sum += f * f
	}
	rms := math.Sqrt(sum/float64(len(samples))) / 32768.0 * 3.0
	if rms > 1.0 {
		rms = 1.0
	}
	s.currentAmp.Store(math.Float64bits(rms))
}

func (s *pulseSpeaker) Play(pcm []byte) error {
	select {
	case s.chunks <- pcm:
	default:
	}
	return nil
}

// Flush clears buffered audio immediately for barge-in interrupts.
// The ~50ms PlaybackLatency residual in the server buffer is acceptable.
func (s *pulseSpeaker) Flush() {
	s.mu.Lock()
	s.buffer = nil
	s.mu.Unlock()
drain:
	for {
		select {
		case <-s.chunks:
		default:
			break drain
		}
	}
}

func (s *pulseSpeaker) CurrentAmplitude() float64 {
	return math.Float64frombits(s.currentAmp.Load())
}

func (s *pulseSpeaker) Close() error {
	s.closeOnce.Do(func() {
		s.stream.Stop()
		s.stream.Close()
		s.client.Close()
	})
	return nil
}
