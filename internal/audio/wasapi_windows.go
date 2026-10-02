//go:build windows

package audio

import (
	"errors"
	"fmt"
	"log/slog"
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
	// failed is set when the device went away and none came back, or a capture call or emit failed, which ends the stream early.
	failed atomic.Bool
}

// wasapiPoll is how long the read loop sleeps between drains. Each chunk handed to emit is what one drain collected, so it is also roughly the chunk length.
const wasapiPoll = 20 * time.Millisecond

// wasapiBuffer is the shared-mode buffer asked for, one second in 100 ns units: far more than one poll, so a late wake-up loses nothing.
const wasapiBuffer = 10_000_000

// reopenFor is how long a stream keeps trying to reopen on the default device after losing its own. A Bluetooth headset switching between its music and call profiles leaves no default device for a second or two.
const reopenFor = 5 * time.Second

// deviceCheck is how often a stream asks Windows which device is the default now, so a headset plugged in mid-call is followed rather than the old device recorded as silence.
const deviceCheck = time.Second

// audclntDeviceInvalidated is AUDCLNT_E_DEVICE_INVALIDATED, the HRESULT a capture client returns once its device is unplugged, disabled or reconfigured.
const audclntDeviceInvalidated = 0x88890004

// endpoint is one opened and started device: its id, its audio client, the capture client reading from it, and the converter for its packets.
type endpoint struct {
	id   string
	ac   *wca.IAudioClient
	acc  *wca.IAudioCaptureClient
	conv *converter
}

func (e *endpoint) close() {
	e.ac.Stop()
	e.acc.Release()
	e.ac.Release()
}

// startWASAPI opens the default endpoint for flow (wca.ECapture for a microphone, wca.ERender to record what that device plays) and role (wca.EConsole or wca.ECommunications), and calls emit on the capture thread with each drain's worth of 16-bit little-endian mono PCM at rate until Close.
// When the device goes away or another device becomes the default, the stream reopens on the current default device and keeps calling the same emit, with the time it was away written as silence. When no device comes back within reopenFor, the stream ends and reports failed.
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
		var de *wca.IMMDeviceEnumerator
		if err := wca.CoCreateInstance(wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL, wca.IID_IMMDeviceEnumerator, &de); err != nil {
			startErr <- fmt.Errorf("create device enumerator: %w", err)
			return
		}
		defer de.Release()
		ep, err := openWASAPI(de, flow, role, rate)
		if err != nil {
			startErr <- err
			return
		}
		s.started = time.Now()
		clock := packetClock{rate: rate, next: qpcHNS()}
		startErr <- nil
		for {
			reopen := s.read(de, ep, &clock, flow, role, emit)
			ep.close()
			if !reopen {
				return
			}
			if ep = s.reopen(de, flow, role, rate); ep == nil {
				return
			}
		}
	}()
	if err := <-startErr; err != nil {
		<-s.done
		return nil, err
	}
	return s, nil
}

// reopen opens the current default device after the stream lost its own, trying every second for reopenFor. Input: the enumerator, flow, role and rate as for startWASAPI. Output: the new endpoint, or nil when Close was called or no device came back, in which case the stream is marked failed.
func (s *wasapiStream) reopen(de *wca.IMMDeviceEnumerator, flow, role uint32, rate int) *endpoint {
	deadline := time.Now().Add(reopenFor)
	for {
		ep, err := openWASAPI(de, flow, role, rate)
		if err == nil {
			slog.Info("audio stream moved to the current default device", "device", ep.id)
			return ep
		}
		if time.Now().After(deadline) {
			slog.Warn("audio device went away and no default device came back", "error", err)
			s.failed.Store(true)
			return nil
		}
		select {
		case <-s.stop:
			return nil
		case <-time.After(time.Second):
		}
	}
}

// defaultID returns the id of the default device for flow and role, or "" when there is none.
func defaultID(de *wca.IMMDeviceEnumerator, flow, role uint32) string {
	var dev *wca.IMMDevice
	if err := de.GetDefaultAudioEndpoint(flow, role, &dev); err != nil {
		return ""
	}
	defer dev.Release()
	var id string
	if err := dev.GetId(&id); err != nil {
		return ""
	}
	return id
}

// openWASAPI activates the default endpoint, initialises it in shared mode and starts it. It first asks Windows to convert to 16-bit mono at rate itself; when the device refuses that, it takes the device's own mix format and converts in Go.
// Input: the device enumerator, and flow, role and rate as for startWASAPI. Output: the started endpoint, or an error naming the step that failed.
func openWASAPI(de *wca.IMMDeviceEnumerator, flow, role uint32, rate int) (*endpoint, error) {
	var dev *wca.IMMDevice
	if err := de.GetDefaultAudioEndpoint(flow, role, &dev); err != nil {
		return nil, fmt.Errorf("no default audio device: %w", err)
	}
	defer dev.Release()
	var id string
	if err := dev.GetId(&id); err != nil {
		return nil, fmt.Errorf("read audio device id: %w", err)
	}

	var flags uint32
	if flow == wca.ERender {
		flags = wca.AUDCLNT_STREAMFLAGS_LOOPBACK
	}
	// A client whose Initialize failed is not reused: a fresh one is activated for the fallback.
	var firstErr error
	for _, direct := range []bool{true, false} {
		var ac *wca.IAudioClient
		if err := dev.Activate(wca.IID_IAudioClient, wca.CLSCTX_ALL, nil, &ac); err != nil {
			return nil, fmt.Errorf("activate audio client: %w", err)
		}
		conv, err := initClient(ac, flags, rate, direct)
		if err != nil {
			ac.Release()
			if firstErr == nil {
				firstErr = err
				continue
			}
			return nil, fmt.Errorf("initialise audio client: %w (converting in Windows: %v)", err, firstErr)
		}
		var acc *wca.IAudioCaptureClient
		if err := ac.GetService(wca.IID_IAudioCaptureClient, &acc); err != nil {
			ac.Release()
			return nil, fmt.Errorf("get capture client: %w", err)
		}
		if err := ac.Start(); err != nil {
			acc.Release()
			ac.Release()
			return nil, fmt.Errorf("start audio client: %w", err)
		}
		return &endpoint{id: id, ac: ac, acc: acc, conv: conv}, nil
	}
	return nil, firstErr
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
		return &converter{block: 2, from: rate, direct: true}, nil
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
		from:     int(mix.NSamplesPerSec),
		channels: int(mix.NChannels),
		float:    float,
		rs:       resampler{from: int(mix.NSamplesPerSec), to: rate},
	}, nil
}

// read drains ep every wasapiPoll until Close, a failure, or a reason to reopen. Silence is written into the gaps where the device delivered nothing, which a loopback stream does while nothing plays.
// Input: the enumerator, the endpoint, the stream's clock, flow and role, and emit. Output: true when the stream should reopen on the current default device, because its own device was invalidated or another device became the default; false when it was closed or failed, in which case failed is set.
func (s *wasapiStream) read(de *wca.IMMDeviceEnumerator, ep *endpoint, clock *packetClock, flow, role uint32, emit func([]byte) error) bool {
	checked := time.Now()
	for {
		select {
		case <-s.stop:
			return false
		case <-time.After(wasapiPoll):
		}
		if time.Since(checked) >= deviceCheck {
			checked = time.Now()
			if id := defaultID(de, flow, role); id != "" && id != ep.id {
				return true
			}
		}
		var chunk []byte
		for {
			// GetBuffer on an empty buffer returns AUDCLNT_S_BUFFER_EMPTY, which go-wca reports as an error, so the packet size is asked first.
			var frames uint32
			if err := ep.acc.GetNextPacketSize(&frames); err != nil {
				return s.lost(err)
			}
			if frames == 0 {
				break
			}
			var data *byte
			var flags uint32
			var devPos, qpc uint64
			if err := ep.acc.GetBuffer(&data, &frames, &flags, &devPos, &qpc); err != nil {
				return s.lost(err)
			}
			n := int(frames) * ep.conv.block
			var pcm []byte
			if flags&wca.AUDCLNT_BUFFERFLAGS_SILENT != 0 {
				// A silent packet's data is to be ignored, not read.
				pcm = ep.conv.convert(make([]byte, n))
			} else {
				pcm = ep.conv.convert(unsafe.Slice(data, n))
			}
			ep.acc.ReleaseBuffer(frames)
			if gap := clock.silenceBefore(int64(qpc), int(frames), ep.conv.from, flags&wca.AUDCLNT_BUFFERFLAGS_TIMESTAMP_ERROR != 0, qpcHNS()); gap > 0 {
				chunk = append(chunk, make([]byte, gap*2)...)
			}
			chunk = append(chunk, pcm...)
		}
		if len(chunk) > 0 {
			if err := emit(chunk); err != nil {
				s.failed.Store(true)
				return false
			}
		}
	}
}

// lost decides what a capture error means. Input: the error from the capture client. Output: true to reopen when the device was invalidated; otherwise false, with failed set.
func (s *wasapiStream) lost(err error) bool {
	var oe *ole.OleError
	if errors.As(err, &oe) && uint32(oe.Code()) == audclntDeviceInvalidated {
		return true
	}
	s.failed.Store(true)
	return false
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
