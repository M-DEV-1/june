//go:build windows

package audio

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
	"golang.org/x/sys/windows"
)

// wasapiStream is one running WASAPI capture: a microphone, or the loopback of a playback device. It reads on its own locked OS thread, because COM objects belong to the thread that made them.
type wasapiStream struct {
	// started is the wall-clock instant the device began capturing.
	started time.Time
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
	// failed is set when the device went away or emit refused a chunk, which ends the stream early.
	failed atomic.Bool
}

// wasapiPoll is how long the read loop sleeps between drains. Each chunk handed to emit is what one drain collected, so it is also roughly the chunk length.
const wasapiPoll = 20 * time.Millisecond

// wasapiBuffer is the shared-mode buffer asked for, one second in 100 ns units: far more than one poll, so a late wake-up loses nothing.
const wasapiBuffer = 10_000_000

// startWASAPI opens the default endpoint for flow (wca.ECapture for a microphone, wca.ERender to record what that device plays) and role (wca.EConsole or wca.ECommunications), and calls emit on the capture thread with each drain's worth of 16-bit little-endian mono PCM at rate until Close.
// Input: flow, role, the sample rate wanted, and emit, whose error stops the stream. Output: the running stream once the device has started, or the error that kept it from starting.
func startWASAPI(flow, role uint32, rate int, emit func([]byte) error) (*wasapiStream, error) {
	s := &wasapiStream{stop: make(chan struct{}), done: make(chan struct{})}
	startErr := make(chan error, 1)
	go func() {
		defer close(s.done)
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); err != nil {
			var oe *ole.OleError
			// S_FALSE means COM was already initialised on this thread, which still wants the matching CoUninitialize.
			if !errors.As(err, &oe) || oe.Code() != 1 {
				startErr <- fmt.Errorf("initialise COM: %w", err)
				return
			}
		}
		defer ole.CoUninitialize()
		ac, acc, conv, err := openWASAPI(flow, role, rate)
		if err != nil {
			startErr <- err
			return
		}
		defer ac.Release()
		defer acc.Release()
		if err := ac.Start(); err != nil {
			startErr <- fmt.Errorf("start audio client: %w", err)
			return
		}
		defer ac.Stop()
		s.started = time.Now()
		startHNS := qpcHNS()
		startErr <- nil
		s.read(acc, conv, rate, flow == wca.ERender, startHNS, emit)
	}()
	if err := <-startErr; err != nil {
		<-s.done
		return nil, err
	}
	return s, nil
}

// openWASAPI activates the default endpoint and initialises it in shared mode. It first asks Windows to convert to 16-bit mono at rate itself; when the device refuses that, it takes the device's own mix format and converts in Go.
// Input: flow, role and rate as for startWASAPI. Output: the audio client, its capture client and the converter for its packets, or an error naming the step that failed.
func openWASAPI(flow, role uint32, rate int) (*wca.IAudioClient, *wca.IAudioCaptureClient, *converter, error) {
	var de *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL, wca.IID_IMMDeviceEnumerator, &de); err != nil {
		return nil, nil, nil, fmt.Errorf("create device enumerator: %w", err)
	}
	defer de.Release()
	var dev *wca.IMMDevice
	if err := de.GetDefaultAudioEndpoint(flow, role, &dev); err != nil {
		return nil, nil, nil, fmt.Errorf("no default audio device: %w", err)
	}
	defer dev.Release()

	var flags uint32
	if flow == wca.ERender {
		flags = wca.AUDCLNT_STREAMFLAGS_LOOPBACK
	}
	// A client whose Initialize failed is not reused: a fresh one is activated for the fallback.
	var firstErr error
	for _, direct := range []bool{true, false} {
		var ac *wca.IAudioClient
		if err := dev.Activate(wca.IID_IAudioClient, wca.CLSCTX_ALL, nil, &ac); err != nil {
			return nil, nil, nil, fmt.Errorf("activate audio client: %w", err)
		}
		conv, err := initClient(ac, flags, rate, direct)
		if err != nil {
			ac.Release()
			if firstErr == nil {
				firstErr = err
				continue
			}
			return nil, nil, nil, fmt.Errorf("initialise audio client: %w (converting in Windows: %v)", err, firstErr)
		}
		var acc *wca.IAudioCaptureClient
		if err := ac.GetService(wca.IID_IAudioCaptureClient, &acc); err != nil {
			ac.Release()
			return nil, nil, nil, fmt.Errorf("get capture client: %w", err)
		}
		return ac, acc, conv, nil
	}
	return nil, nil, nil, firstErr
}

// initClient initialises ac in shared mode. Direct asks for 16-bit mono at rate with Windows' own converter; otherwise the device's mix format is used as is, which must be 32-bit float or 16-bit integer.
// Input: the client, the extra stream flags (loopback or none), the rate wanted and which attempt this is. Output: the converter for the packets this client will deliver.
func initClient(ac *wca.IAudioClient, flags uint32, rate int, direct bool) (*converter, error) {
	if direct {
		want := &wca.WAVEFORMATEX{
			WFormatTag:      wca.WAVE_FORMAT_PCM,
			NChannels:       1,
			NSamplesPerSec:  uint32(rate),
			NAvgBytesPerSec: uint32(rate * 2),
			NBlockAlign:     2,
			WBitsPerSample:  16,
		}
		flags |= wca.AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM | wca.AUDCLNT_STREAMFLAGS_SRC_DEFAULT_QUALITY
		if err := ac.Initialize(wca.AUDCLNT_SHAREMODE_SHARED, flags, wasapiBuffer, 0, want, nil); err != nil {
			return nil, err
		}
		return &converter{block: 2, direct: true}, nil
	}
	var mix *wca.WAVEFORMATEX
	if err := ac.GetMixFormat(&mix); err != nil {
		return nil, fmt.Errorf("read mix format: %w", err)
	}
	defer ole.CoTaskMemFree(uintptr(unsafe.Pointer(mix)))
	// The tag says float (3) or integer (1) directly, or 0xFFFE defers to the WAVEFORMATEXTENSIBLE SubFormat GUID, whose first four bytes sit 24 bytes into the struct and carry the same 3 or 1.
	kind := uint32(mix.WFormatTag)
	if kind == 0xFFFE {
		kind = *(*uint32)(unsafe.Add(unsafe.Pointer(mix), 24))
	}
	float := kind == 3 && mix.WBitsPerSample == 32
	if !float && !(kind == 1 && mix.WBitsPerSample == 16) {
		return nil, fmt.Errorf("unsupported mix format: tag %d, %d bits", kind, mix.WBitsPerSample)
	}
	if err := ac.Initialize(wca.AUDCLNT_SHAREMODE_SHARED, flags, wasapiBuffer, 0, mix, nil); err != nil {
		return nil, err
	}
	return &converter{
		block:    int(mix.NBlockAlign),
		channels: int(mix.NChannels),
		float:    float,
		rs:       resampler{from: int(mix.NSamplesPerSec), to: rate},
	}, nil
}

// read drains the capture client every wasapiPoll until Close or a failure. A loopback stream gets silence written into the gaps where nothing was playing, since WASAPI delivers no packets then.
func (s *wasapiStream) read(acc *wca.IAudioCaptureClient, conv *converter, rate int, loopback bool, startHNS int64, emit func([]byte) error) {
	var written int64
	for {
		select {
		case <-s.stop:
			return
		case <-time.After(wasapiPoll):
		}
		var chunk []byte
		for {
			// GetBuffer on an empty buffer returns AUDCLNT_S_BUFFER_EMPTY, which go-wca reports as an error, so the packet size is asked first.
			var frames uint32
			if err := acc.GetNextPacketSize(&frames); err != nil {
				s.failed.Store(true)
				return
			}
			if frames == 0 {
				break
			}
			var data *byte
			var flags uint32
			var devPos, qpc uint64
			if err := acc.GetBuffer(&data, &frames, &flags, &devPos, &qpc); err != nil {
				s.failed.Store(true)
				return
			}
			n := int(frames) * conv.block
			var pcm []byte
			if flags&wca.AUDCLNT_BUFFERFLAGS_SILENT != 0 {
				// A silent packet's data is to be ignored, not read.
				pcm = conv.convert(make([]byte, n))
			} else {
				pcm = conv.convert(unsafe.Slice(data, n))
			}
			acc.ReleaseBuffer(frames)
			if loopback {
				if gap := gapFrames(int64(qpc), startHNS, rate, written); gap > 0 {
					chunk = append(chunk, make([]byte, gap*2)...)
					written += gap
				}
			}
			chunk = append(chunk, pcm...)
			written += int64(len(pcm) / 2)
		}
		if len(chunk) > 0 {
			if err := emit(chunk); err != nil {
				s.failed.Store(true)
				return
			}
		}
	}
}

// Close stops the stream and waits for its thread to finish, so emit is never called once Close has returned.
func (s *wasapiStream) Close() {
	s.once.Do(func() { close(s.stop) })
	<-s.done
}

var (
	procQPC  = windows.NewLazySystemDLL("kernel32.dll").NewProc("QueryPerformanceCounter")
	procQPF  = windows.NewLazySystemDLL("kernel32.dll").NewProc("QueryPerformanceFrequency")
	qpcFreqs sync.Once
	qpcFreq  int64
)

// qpcHNS reads the performance counter in 100 ns units, the clock GetBuffer stamps each packet with.
func qpcHNS() int64 {
	qpcFreqs.Do(func() { procQPF.Call(uintptr(unsafe.Pointer(&qpcFreq))) })
	var c int64
	procQPC.Call(uintptr(unsafe.Pointer(&c)))
	if qpcFreq == 0 {
		return 0
	}
	return c/qpcFreq*10_000_000 + c%qpcFreq*10_000_000/qpcFreq
}
