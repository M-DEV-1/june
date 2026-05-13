//go:build windows

package audio

import (
	"context"
	"fmt"
	"runtime"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
)

type winMic struct{}

func NewMic() (Microphone, error) {
	// must initialize COM for the entire audio engine
	// go-ole is bridge between go and msft Component Object Model 1993, universal translator of sorts
	// new com thread

	if err := ole.CoInitialize(0); err != nil {
		return nil, fmt.Errorf("failed to init COM: %w", err)
	}
	return &winMic{}, nil
}

func (m *winMic) StartCapture(ctx context.Context) (<-chan []byte, error) {

	micChan := make(chan []byte, 100)

	// first go-routine written in this codebase
	// god bless
	// number of hours spent here: 1
	// earlier this was a 100 line goroutine, and that was hella suspicious plus it mixed hardware inits plus thread locks. rewrote with helpers.
	go func() {
		// com threading rules - needs to be on the same thread ofc
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		if err := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); err != nil {
			close(micChan)
			return
		}
		defer ole.CoUninitialize()

		// hardware setup using our super cool helper func
		// ac - audioclient, acc - audiocaptureclient
		ac, acc, err := setupAudioHardware()
		if err != nil {
			fmt.Printf("Hardware setup failed: %v\n", err)
			close(micChan)
			return
		}
		defer ac.Release()
		defer acc.Release()
		defer ac.Stop()
		defer close(micChan)

		// inf read loop
		// inf seemed dangerous at first because i'm playing with threads here
		for {
			select {
			case <-ctx.Done(): // stop listening altogether (llm resp, or sigint)
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

				if frames > 0 {
					floatData := unsafe.Slice((*float32)(unsafe.Pointer(data)), frames)
					pcm := make([]byte, frames*2)

					for i := 0; i < int(frames); i++ {
						val := float32ToInt16(floatData[i])
						pcm[i*2] = byte(val)
						pcm[i*2+1] = byte(val >> 8)
					}

					select {
					case micChan <- pcm:
					case <-ctx.Done():
						return
					}
				}
				acc.ReleaseBuffer(frames)
			}
		}
	}()

	return micChan, nil
}

func (m *winMic) Close() error {
	ole.CoUninitialize()
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
