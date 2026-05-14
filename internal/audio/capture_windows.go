//go:build windows

package audio

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
	"go.opentelemetry.io/otel"
)
type winMic struct {
	mu          sync.Mutex
	isCapturing bool
	cancel      context.CancelFunc
}
type winMic struct{}

func NewMic() (Microphone, error) {
	// must initialize COM for the entire audio engine
	// go-ole is bridge between go and msft Component Object Model 1993, universal translator of sorts
	// new com thread
	// com is strictly per-thread

	// if err := ole.CoInitialize(0); err != nil {
	// 	return nil, fmt.Errorf("failed to init COM: %w", err)
	// }
	// initializes COM with a reserved param 0, does nothing. pvReversed in docs or smth
	return &winMic{}, nil
}

func (m *winMic) StartCapture(ctx context.Context) (<-chan []byte, error) {
	// wasapi windows audio sys
	// this is very picky, prone to crashing or zombie threads in subsequent repeated calls (from what i read), hence the mutex
	m.mu.Lock()
	if m.isCapturing {
		m.mu.Unlock()
		return nil, fmt.Errorf("microphone is already capturing")
	}
	m.isCapturing = true
	m.mu.Unlock()

	// cancellable context allows us to stop goroutine with close()
	ctx, m.cancel = context.WithCancel(ctx)

	tracer := otel.Tracer("ora.audio")
	setupCtx, span := tracer.Start(ctx, "Mic.StartCaptureSetup")

	micChan := make(chan []byte, 100)
	// we are forced to init hardware inside goroutine bcz com is per-thread
	// 1 slot error channel that we ill use to return error to main thread
	startupErr := make(chan error, 1)

	// first go-routine written in this codebase
	// god bless
	// number of hours spent here: 3
	// earlier this was a 100 line goroutine, and that was hella suspicious plus it mixed hardware inits plus thread locks. rewrote with helpers.
	go func() {
		// com threading rules - needs to be on the same thread ofc
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		if err := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); err != nil {
			startupErr <- err
			close(micChan)
			return
		}
		defer ole.CoUninitialize()

		// hardware setup using our super cool helper func
		// ac - audioclient, acc - audiocaptureclient
		ac, acc, err := setupAudioHardware()
		if err != nil {
			startupErr <- fmt.Errorf("Hardware setup failed: %w\n", err)
			close(micChan)
			return
		}
		defer ac.Release()
		defer acc.Release()
		defer ac.Stop()
		defer close(micChan)

		// signalling success to main thread
		startupErr <- nil

		// inf read loop
		// inf seemed dangerous at first because i'm playing with threads here
		for {
			select {
			case <-setupCtx.Done(): // stop listening altogether (llm resp, or sigint)
				return
			default:
				var frames uint32
				var data *byte
				var flags uint32

				err := acc.GetBuffer(&data, &frames, &flags, nil, nil)
				if err != nil {
					time.Sleep(10 * time.Millisecond)
					// let the cpu rest ong
					continue
				}

				var pcm []byte

				if frames > 0 {
					floatData := unsafe.Slice((*float32)(unsafe.Pointer(data)), frames)
					pcm = make([]byte, frames*2)

					for i := 0; i < int(frames); i++ {
						val := float32ToInt16(floatData[i])
						pcm[i*2] = byte(val)
						pcm[i*2+1] = byte(val >> 8)
					}

					// releasing gives memory back to soundcard before we block further audio
					acc.ReleaseBuffer(frames)
					select {
					case micChan <- pcm:
					case <-ctx.Done():
						return
					}
				} else {
					acc.ReleaseBuffer(0)
				}
			}
		}
	}()

	err := <-startupErr
	span.End()
	if err != nil {
		return nil, err // at least now we are passing the exact hardware error back into caller
	}

	return micChan, nil
}

func (m *winMic) Close() error {
	// ole.CoUninitialize()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.isCapturing = false

	if m.cancel != nil {
		m.cancel()
	}
	return nil
}

// helps init hardware and return to client where we read from
// this was painful to write
// all windows core audio setup lives here
func setupAudioHardware() (*wca.IAudioClient, *wca.IAudioCaptureClient, error) {
	var enumerator *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL, wca.IID_IMMDeviceEnumerator, &enumerator); err != nil {
		return nil, nil, err
	}
	defer enumerator.Release()

	var mmde *wca.IMMDevice
	if err := enumerator.GetDefaultAudioEndpoint(wca.ECapture, wca.EConsole, &mmde); err != nil {
		return nil, nil, err
	}
	defer mmde.Release()

	var ac *wca.IAudioClient
	if err := mmde.Activate(wca.IID_IAudioClient, wca.CLSCTX_ALL, nil, &ac); err != nil {
		return nil, nil, err
	}

	format := &wca.WAVEFORMATEX{
		WFormatTag:      3, //wca.WAVE_FORMAT_IEEE_FLOAT
		NChannels:       1,
		NSamplesPerSec:  24000,
		NAvgBytesPerSec: 24000 * 4,
		NBlockAlign:     4,
		WBitsPerSample:  32,
		CbSize:          0,
	}

	if err := ac.Initialize(wca.AUDCLNT_SHAREMODE_SHARED, wca.AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM, 10000000, 0, format, nil); err != nil {
		return nil, nil, err
	}

	var acc *wca.IAudioCaptureClient
	if err := ac.GetService(wca.IID_IAudioCaptureClient, &acc); err != nil {
		return nil, nil, err
	}

	if err := ac.Start(); err != nil {
		acc.Release()
		ac.Release()
		return nil, nil, err
	}

	return ac, acc, nil
}

// f32 to i16 transcoder
// windows provides us with floats, and the current audio model (gemini) requires int
// function co-authored by Gemini 3.1 Pro
func float32ToInt16(f float32) int16 {
	if f > 1.0 {
		f = 1.0
	} else if f < -1.0 {
		f = -1.0
	}
	return int16(f * 32767)
}
