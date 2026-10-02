//go:build windows

package audio

import (
	"context"
	"fmt"
	"math"
	"sync"
	"sync/atomic"

	"github.com/moutend/go-wca/pkg/wca"
)

// micRate matches the Linux microphone: 24 kHz mono s16le, which is what the voice agent and dictation expect.
const micRate = 24000

// winMic is the default Windows microphone through WASAPI shared mode.
// ponytail: no echo cancellation, so in live voice the mic can hear June's own replies through speakers; Linux gets this from PipeWire's echo-cancel module, Windows could ask the driver for it with IAudioClient2.SetClientProperties and AudioCategory_Communications.
type winMic struct {
	mu          sync.Mutex
	isCapturing bool
	cancel      context.CancelFunc
	currentAmp  atomic.Uint64
}

// NewMic returns an unopened mic; the device is opened in StartCapture.
func NewMic() (Microphone, error) {
	return &winMic{}, nil
}

// StartCapture opens the default capture device and returns a channel of 24 kHz mono s16le chunks of about 20 ms. The channel closes once ctx is cancelled or Close is called. A chunk the consumer is not ready for is dropped rather than blocking the capture thread.
func (m *winMic) StartCapture(ctx context.Context) (<-chan []byte, error) {
	m.mu.Lock()
	if m.isCapturing {
		m.mu.Unlock()
		return nil, fmt.Errorf("microphone is already capturing")
	}
	m.isCapturing = true
	m.mu.Unlock()

	micChan := make(chan []byte, 100)
	stream, err := startWASAPI(wca.ECapture, wca.EConsole, micRate, func(pcm []byte) error {
		m.currentAmp.Store(math.Float64bits(level(pcm)))
		select {
		case micChan <- pcm:
		default:
		}
		return nil
	})
	if err != nil {
		m.mu.Lock()
		m.isCapturing = false
		m.mu.Unlock()
		return nil, err
	}

	captureCtx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	m.cancel = cancel
	m.mu.Unlock()
	go func() {
		<-captureCtx.Done()
		// Close waits for the capture thread, so nothing sends on micChan after it is closed.
		stream.Close()
		close(micChan)
		m.mu.Lock()
		m.isCapturing = false
		m.mu.Unlock()
	}()
	return micChan, nil
}

func (m *winMic) CurrentAmplitude() float64 {
	return math.Float64frombits(m.currentAmp.Load())
}

func (m *winMic) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil {
		m.cancel()
	}
	return nil
}
