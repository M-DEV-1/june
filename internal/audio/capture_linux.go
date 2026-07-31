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
}

// NewMic returns an uninitialised mic; hardware is connected lazily in StartCapture.
func NewMic() (Microphone, error) {
	return &pulseMic{}, nil
}

func (m *pulseMic) StartCapture(ctx context.Context) (<-chan []byte, error) {
	m.mu.Lock()
	if m.isCapturing {
		m.mu.Unlock()
		return nil, fmt.Errorf("microphone is already capturing")
	}
	m.isCapturing = true
	m.mu.Unlock()

	c, err := pulse.NewClient()
	if err != nil {
		m.mu.Lock()
		m.isCapturing = false
		m.mu.Unlock()
		return nil, err
	}

	captureCtx, cancel := context.WithCancel(ctx)
	micChan := make(chan []byte, 100)

	m.mu.Lock()
	m.client = c
	m.cancel = cancel
	m.micChan = micChan
	m.mu.Unlock()

	// RecordLatency is required: PipeWire's pulse server delivers no data to a record stream that leaves buffer attributes unset (fragment/latency).
	stream, err := c.NewRecord(
		pulse.Int16Writer(m.writeFn),
		pulse.RecordSampleRate(24000),
		pulse.RecordChannels(proto.ChannelMap{proto.ChannelMono}),
		pulse.RecordLatency(0.05),
	)
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
// Converts to s16le bytes, stores amplitude, and forwards non-blocking.
func (m *pulseMic) writeFn(in []int16) (int, error) {
	if len(in) == 0 {
		return 0, nil
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
	var sum float64
	for _, s := range samples {
		f := float64(s)
		sum += f * f
	}
	rms := math.Sqrt(sum/float64(len(samples))) / 32768.0 * 3.0
	if rms > 1.0 {
		rms = 1.0
	}
	m.currentAmp.Store(math.Float64bits(rms))
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
