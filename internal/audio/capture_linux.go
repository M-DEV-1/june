//go:build linux

package audio

import (
	"context"
	"fmt"
	"math"
	"sync"
	"sync/atomic"

	"github.com/jfreymuth/pulse"
	"github.com/jfreymuth/pulse/proto"
)

type pulseMic struct {
	mu          sync.Mutex
	isCapturing bool
	client      *pulse.Client
	stream      *pulse.RecordStream
	micChan     chan []byte
	cancel      context.CancelFunc
	currentAmp  atomic.Uint64
	// voice marks the voice session's mic, which gets a fresh echoGuard on every StartCapture. Dictation and the mic test leave guard nil: the user starts speaking right after the key press, which the open-time mute would cut.
	voice bool
	guard *echoGuard
}

// NewMic returns an uninitialised mic; hardware is connected lazily in StartCapture.
// EchoCancelSource and EchoCancelSink name the virtual source and sink PipeWire's echo-cancel module creates when loaded with June's config. The mic records from the source and the speaker plays through the sink, which is what gives the canceller its reference signal.
const (
	EchoCancelSource = "june_ec_source"
	EchoCancelSink   = "june_ec_sink"
)

func NewMic() (Microphone, error) {
	return &pulseMic{}, nil
}

// NewVoiceMic is NewMic for the voice session: each StartCapture also mutes the first second and silences chunks that are only June's own voice coming back (see echoGuard).
func NewVoiceMic() (Microphone, error) {
	return &pulseMic{voice: true}, nil
}

func (m *pulseMic) StartCapture(ctx context.Context) (<-chan []byte, error) {
	m.mu.Lock()
	if m.isCapturing {
		m.mu.Unlock()
		return nil, fmt.Errorf("microphone is already capturing")
	}
	m.isCapturing = true
	m.mu.Unlock()

	// Named so the stream says whose it is. Left unnamed, the audio library falls back to the binary's own name, and June's meeting watcher — which asks whether to record whenever something takes the microphone — could not tell the assistant listening from a call starting.
	c, err := pulse.NewClient(pulse.ClientApplicationName("June voice"))
	if err != nil {
		m.mu.Lock()
		m.isCapturing = false
		m.mu.Unlock()
		return nil, err
	}

	captureCtx, cancel := context.WithCancel(ctx)
	micChan := make(chan []byte, 100)

	m.mu.Lock()
	if m.voice {
		m.guard = newEchoGuard()
	}
	m.client = c
	m.cancel = cancel
	m.micChan = micChan
	m.mu.Unlock()

	// RecordLatency is required: PipeWire's pulse server delivers no data to a record stream that leaves buffer attributes unset (fragment/latency).
	opts := []pulse.RecordOption{
		pulse.RecordSampleRate(24000),
		pulse.RecordChannels(proto.ChannelMap{proto.ChannelMono}),
		pulse.RecordLatency(0.05),
	}
	// The echo-cancelled source, when the audio server has one (see ~/.config/pipewire/pipewire.conf.d/99-june-echo-cancel.conf). It subtracts whatever is played through EchoCancelSink and suppresses room noise, so the mic stops hearing June's own replies and answering them; recording from the default mic instead is what looped a session on 2026-09-07. Absent, the default mic is used as before.
	if src, err := c.SourceByID(EchoCancelSource); err == nil {
		opts = append(opts, pulse.RecordSource(src))
	}
	stream, err := c.NewRecord(pulse.Int16Writer(m.writeFn), opts...)
	if err != nil {
		cancel()
		c.Close()
		m.mu.Lock()
		m.isCapturing = false
		m.mu.Unlock()
		return nil, err
	}

	m.mu.Lock()
	m.stream = stream
	m.mu.Unlock()

	stream.Start()

	// goroutine tears down the stream when the caller cancels the context
	go func() {
		<-captureCtx.Done()
		stream.Stop()
		stream.Close()
		c.Close()
		close(micChan)
		m.mu.Lock()
		m.isCapturing = false
		m.mu.Unlock()
	}()

	return micChan, nil
}

// writeFn is called by the pulse library with each captured batch of samples.
// Silences echo when the mic has a guard, converts to s16le bytes, stores amplitude, and forwards non-blocking.
func (m *pulseMic) writeFn(in []int16) (int, error) {
	if len(in) == 0 {
		return 0, nil
	}
	if m.guard != nil {
		m.guard.process(in, takePlaybackPeak())
	}

	buf := make([]byte, len(in)*2)
	for i, s := range in {
		buf[i*2] = byte(s)
		buf[i*2+1] = byte(s >> 8)
	}

	m.storeAmp(in)

	select {
	case m.micChan <- buf:
	default:
		// drop frame rather than block when consumer is slow
	}
	return len(in), nil
}

func (m *pulseMic) storeAmp(samples []int16) {
	if len(samples) == 0 {
		return
	}
	m.currentAmp.Store(math.Float64bits(math.Min(sampleRMS(samples)/32768.0*3.0, 1.0)))
}

func (m *pulseMic) CurrentAmplitude() float64 {
	return math.Float64frombits(m.currentAmp.Load())
}

func (m *pulseMic) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.isCapturing = false
	if m.cancel != nil {
		m.cancel()
	}
	return nil
}
