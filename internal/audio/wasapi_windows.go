//go:build windows

package audio

import (
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
	"golang.org/x/sys/windows"
)

// wasapiStream is one running WASAPI capture: a microphone, or the loopback of a playback device. It reads on its own locked OS thread (see onCOMThread).
type wasapiStream struct {
	// started is the wall-clock instant the device began capturing.
	started time.Time
	// live is set for a stream feeding a conversation rather than a recording (see startWASAPI).
	live bool
	stop chan struct{}
	done chan struct{}
	once sync.Once
	// failed is set when the device went away and none came back, or a capture call or emit failed, which ends the stream early. A live stream only fails on emit.
	failed atomic.Bool
}

// wasapiPoll is how long the read loop sleeps between drains. Each chunk handed to emit is what one drain collected, so it is also roughly the chunk length.
const wasapiPoll = 20 * time.Millisecond

// wasapiBuffer is the shared-mode buffer asked for, one second in 100 ns units: far more than one poll, so a late wake-up loses nothing.
const wasapiBuffer = 10_000_000

// reopenFor is how long a recording keeps trying to reopen on the default device after losing its own. A Bluetooth headset switching between its music and call profiles leaves no default device for a second or two. A live stream and the speaker keep trying until they are closed.
const reopenFor = 5 * time.Second

// reopenFirst and reopenMax bound the wait between tries to reopen: the first comes quickly, for a device that is only switching profiles, and the waits double up to reopenMax for one that is gone for a while.
const (
	reopenFirst = 250 * time.Millisecond
	reopenMax   = 2 * time.Second
)

// deviceCheck is how often a stream asks Windows which device is the default now, so a headset plugged in mid-call is followed rather than the old device recorded as silence.
const deviceCheck = time.Second

// The HRESULTs that mean the device went away rather than that the stream is broken, so opening the default device again is worth it: AUDCLNT_E_DEVICE_INVALIDATED once it is unplugged, disabled or reconfigured, AUDCLNT_E_SERVICE_NOT_RUNNING while the Windows audio service restarts, and AUDCLNT_E_RESOURCES_INVALIDATED when the engine took the stream's resources back.
const (
	audclntDeviceInvalidated    = 0x88890004
	audclntServiceNotRunning    = 0x88890010
	audclntResourcesInvalidated = 0x88890026
)

// errClosed is what retry returns when the stream it was reopening was closed while it waited.
var errClosed = errors.New("audio stream closed")

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

// onCOMThread runs body on a goroutine locked to one OS thread, with COM initialised there and a device enumerator made on it, because COM objects belong to the thread that made them. body reports through started whether its device opened, which is what onCOMThread returns, and after reporting nil it goes on running on that thread for as long as its stream lives.
// Input: done, closed once body has returned and COM is released, and body, which must call started exactly once. Output: the error body reported, or the one that kept COM from starting.
func onCOMThread(done chan struct{}, body func(de *wca.IMMDeviceEnumerator, started func(error))) error {
	startErr := make(chan error, 1)
	go func() {
		defer close(done)
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
		body(de, func(err error) { startErr <- err })
	}()
	if err := <-startErr; err != nil {
		<-done
		return err
	}
	return nil
}

// startWASAPI opens the default endpoint for flow (wca.ECapture for a microphone, wca.ERender to record what that device plays) and role (wca.EConsole or wca.ECommunications), and calls emit on the capture thread with each drain's worth of 16-bit little-endian mono PCM at rate until Close.
// When the device goes away or another device becomes the default, the stream moves to the current default device and keeps calling the same emit. A recording writes the time it was away, and any stretch in which its device delivered nothing (a loopback while nothing plays), as silence, so its file stays on the wall clock; when no device comes back within reopenFor it ends and reports failed. A live stream, the voice microphone, writes no silence and waits for a device for as long as it takes, the way the Linux microphone stays open while PipeWire has nothing to give it: a conversation wants the microphone back, not a record of the gap.
// Input: flow, role, the sample rate wanted, whether the stream is live, and emit, whose error stops the stream. Output: the running stream once the device has started, or the error that kept it from starting.
func startWASAPI(flow, role uint32, rate int, live bool, emit func([]byte) error) (*wasapiStream, error) {
	s := &wasapiStream{live: live, stop: make(chan struct{}), done: make(chan struct{})}
	err := onCOMThread(s.done, func(de *wca.IMMDeviceEnumerator, started func(error)) {
		ep, err := openWASAPI(de, flow, role, rate)
		if err != nil {
			started(err)
			return
		}
		s.started = time.Now()
		lead := sleepLead()
		clock := packetClock{rate: rate, next: qpcHNS(), lead: lead, cut: lead}
		started(nil)
		for {
			next, lost := s.read(de, ep, &clock, flow, role, emit)
			ep.close()
			switch {
			case next != nil:
				slog.Info("audio stream moved to the new default device", "device", next.id)
				ep = next
			case lost:
				if ep = s.reopen(de, flow, role, rate); ep == nil {
					return
				}
			default:
				return
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return s, nil
}

// reopen opens the current default device after the stream lost its own: for reopenFor in a recording, until Close in a live stream. Input: the enumerator, flow, role and rate as for startWASAPI. Output: the new endpoint, or nil when Close was called or a recording's device did not come back, in which case the stream is marked failed.
func (s *wasapiStream) reopen(de *wca.IMMDeviceEnumerator, flow, role uint32, rate int) *endpoint {
	limit := reopenFor
	if s.live {
		limit = 0
	}
	var ep *endpoint
	err := retry(s.stop, limit, func() (err error) {
		ep, err = openWASAPI(de, flow, role, rate)
		return err
	})
	switch {
	case err == nil:
		slog.Info("audio stream moved to the current default device", "device", ep.id)
		return ep
	case errors.Is(err, errClosed):
		return nil
	}
	slog.Warn("audio device went away and no default device came back", "error", err)
	s.failed.Store(true)
	return nil
}

// retry calls open until it succeeds, waiting reopenFirst before the first try and twice as long before each one after that, up to reopenMax. Waiting before the first try too keeps a device that fails as soon as it is opened from being reopened in a tight loop.
// Input: the stream's stop channel, how long to keep trying (0 for as long as it takes) and open. Output: nil once open succeeded, errClosed when stop closed first, or open's last error when the time ran out.
func retry(stop <-chan struct{}, limit time.Duration, open func() error) error {
	deadline := time.Now().Add(limit)
	wait := reopenFirst
	for {
		select {
		case <-stop:
			return errClosed
		case <-time.After(wait):
		}
		err := open()
		if err == nil {
			return nil
		}
		if limit > 0 && time.Now().After(deadline) {
			return err
		}
		wait = min(2*wait, reopenMax)
	}
}

// deviceID reads dev's endpoint id. go-wca's IMMDevice.GetId reads the returned string before it looks at the HRESULT, so a failed call there dereferences a null pointer and takes the daemon down; this calls the method itself and reads the string only when there is one.
// Input: the device. Output: its id, or the error the call returned.
func deviceID(dev *wca.IMMDevice) (string, error) {
	var p *uint16
	hr, _, _ := syscall.SyscallN(dev.VTable().GetId, uintptr(unsafe.Pointer(dev)), uintptr(unsafe.Pointer(&p)))
	if hr != 0 {
		return "", ole.NewError(hr)
	}
	if p == nil {
		return "", errors.New("audio device returned no id")
	}
	defer ole.CoTaskMemFree(uintptr(unsafe.Pointer(p)))
	return windows.UTF16PtrToString(p), nil
}

// defaultID returns the id of the default device for flow and role, or "" when there is none.
func defaultID(de *wca.IMMDeviceEnumerator, flow, role uint32) string {
	var dev *wca.IMMDevice
	if err := de.GetDefaultAudioEndpoint(flow, role, &dev); err != nil {
		return ""
	}
	defer dev.Release()
	id, err := deviceID(dev)
	if err != nil {
		return ""
	}
	return id
}

// activateDefault activates an audio client on the default device for flow and role and initialises it with init, first with direct set (Windows converts to the format wanted) and, when that is refused, on a fresh client with direct unset (the device's own mix format, converted in Go). A client whose Initialize failed is not reused.
// Input: the device enumerator, flow, role and init. Output: the device's id and the initialised client, not yet started, or an error naming the step that failed.
func activateDefault(de *wca.IMMDeviceEnumerator, flow, role uint32, init func(ac *wca.IAudioClient, direct bool) error) (string, *wca.IAudioClient, error) {
	var dev *wca.IMMDevice
	if err := de.GetDefaultAudioEndpoint(flow, role, &dev); err != nil {
		return "", nil, fmt.Errorf("no default audio device: %w", err)
	}
	defer dev.Release()
	id, err := deviceID(dev)
	if err != nil {
		return "", nil, fmt.Errorf("read audio device id: %w", err)
	}
	var firstErr error
	for _, direct := range []bool{true, false} {
		var ac *wca.IAudioClient
		if err := dev.Activate(wca.IID_IAudioClient, wca.CLSCTX_ALL, nil, &ac); err != nil {
			return "", nil, fmt.Errorf("activate audio client: %w", err)
		}
		err := init(ac, direct)
		if err == nil {
			return id, ac, nil
		}
		ac.Release()
		if firstErr == nil {
			firstErr = err
			continue
		}
		return "", nil, fmt.Errorf("initialise audio client: %w (converting in Windows: %v)", err, firstErr)
	}
	return "", nil, firstErr
}

// openWASAPI activates the default endpoint, initialises it in shared mode and starts it. It first asks Windows to convert to 16-bit mono at rate itself; when the device refuses that, it takes the device's own mix format and converts in Go.
// Input: the device enumerator, and flow, role and rate as for startWASAPI. Output: the started endpoint, or an error naming the step that failed.
func openWASAPI(de *wca.IMMDeviceEnumerator, flow, role uint32, rate int) (*endpoint, error) {
	var flags uint32
	if flow == wca.ERender {
		flags = wca.AUDCLNT_STREAMFLAGS_LOOPBACK
	}
	var conv *converter
	id, ac, err := activateDefault(de, flow, role, func(ac *wca.IAudioClient, direct bool) (err error) {
		conv, err = initClient(ac, flags, rate, direct)
		return err
	})
	if err != nil {
		return nil, err
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

// monoFormat is 16-bit mono PCM at rate, what June asks a device for when Windows will do the converting.
func monoFormat(rate int) *wca.WAVEFORMATEX {
	return &wca.WAVEFORMATEX{
		WFormatTag:      wca.WAVE_FORMAT_PCM,
		NChannels:       1,
		NSamplesPerSec:  uint32(rate),
		NAvgBytesPerSec: uint32(rate * 2),
		NBlockAlign:     2,
		WBitsPerSample:  16,
	}
}

// mixFormat reads the format the device mixes in, which a client that does not ask Windows to convert must use as is. Input: the client. Output: the format, for the caller to free with ole.CoTaskMemFree, and whether its samples are 32-bit float rather than 16-bit integer; an error for any other sample type.
func mixFormat(ac *wca.IAudioClient) (*wca.WAVEFORMATEX, bool, error) {
	var mix *wca.WAVEFORMATEX
	if err := ac.GetMixFormat(&mix); err != nil {
		return nil, false, fmt.Errorf("read mix format: %w", err)
	}
	// The tag says float (3) or integer (1) directly, or 0xFFFE defers to the WAVEFORMATEXTENSIBLE SubFormat GUID, whose first four bytes sit 24 bytes into the struct and carry the same 3 or 1.
	kind := uint32(mix.WFormatTag)
	if kind == 0xFFFE {
		kind = *(*uint32)(unsafe.Add(unsafe.Pointer(mix), 24))
	}
	bits := mix.WBitsPerSample
	float := kind == 3 && bits == 32
	if !float && !(kind == 1 && bits == 16) {
		ole.CoTaskMemFree(uintptr(unsafe.Pointer(mix)))
		return nil, false, fmt.Errorf("unsupported mix format: tag %d, %d bits", kind, bits)
	}
	return mix, float, nil
}

// initClient initialises ac in shared mode. Direct asks for 16-bit mono at rate with Windows' own converter; otherwise the device's mix format is used as is, which must be 32-bit float or 16-bit integer.
// Input: the client, the extra stream flags (loopback or none), the rate wanted and which attempt this is. Output: the converter for the packets this client will deliver.
func initClient(ac *wca.IAudioClient, flags uint32, rate int, direct bool) (*converter, error) {
	if direct {
		flags |= wca.AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM | wca.AUDCLNT_STREAMFLAGS_SRC_DEFAULT_QUALITY
		if err := ac.Initialize(wca.AUDCLNT_SHAREMODE_SHARED, flags, wasapiBuffer, 0, monoFormat(rate), nil); err != nil {
			return nil, err
		}
		return &converter{block: 2, from: rate, direct: true}, nil
	}
	mix, float, err := mixFormat(ac)
	if err != nil {
		return nil, err
	}
	defer ole.CoTaskMemFree(uintptr(unsafe.Pointer(mix)))
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

// read drains ep every wasapiPoll until Close, a failure, or a reason to move to another device. A recording also writes silence where its device delivered nothing (see startWASAPI), at most a second of it per call to emit, so a long gap is never one allocation the size of the gap.
// Input: the enumerator, the endpoint, the stream's clock, flow and role, and emit. Output: the endpoint to move to, already started, when another device became the default; lost when ep's device went away and the stream should reopen on whatever is the default now; neither when the stream was closed or failed, in which case failed is set if it failed.
func (s *wasapiStream) read(de *wca.IMMDeviceEnumerator, ep *endpoint, clock *packetClock, flow, role uint32, emit func([]byte) error) (next *endpoint, lost bool) {
	send := func(pcm []byte) bool {
		if len(pcm) == 0 {
			return true
		}
		if err := emit(pcm); err != nil {
			s.failed.Store(true)
			return false
		}
		return true
	}
	quiet := func(frames int64) bool {
		for frames > 0 {
			n := min(frames, int64(clock.rate))
			if !send(make([]byte, n*2)) {
				return false
			}
			frames -= n
		}
		return true
	}
	checked := time.Now()
	var refused string
	for {
		select {
		case <-s.stop:
			return nil, false
		case <-time.After(wasapiPoll):
		}
		if time.Since(checked) >= deviceCheck {
			checked = time.Now()
			if id := defaultID(de, flow, role); id != "" && id != ep.id {
				// The new default is opened before ep is let go, so one that will not open leaves the stream on the device it has rather than on nothing.
				moved, err := openWASAPI(de, flow, role, clock.rate)
				switch {
				case err == nil && moved.id != ep.id:
					return moved, false
				case err == nil:
					moved.close()
				case id != refused:
					slog.Warn("the new default audio device would not open, staying on the current one", "device", id, "error", err)
					refused = id
				}
			}
		}
		var chunk []byte
		got := false
		for {
			// GetBuffer on an empty buffer returns AUDCLNT_S_BUFFER_EMPTY, which go-wca reports as an error, so the packet size is asked first.
			var frames uint32
			if err := ep.acc.GetNextPacketSize(&frames); err != nil {
				return nil, s.lost(err)
			}
			if frames == 0 {
				break
			}
			var data *byte
			var flags uint32
			var devPos, qpc uint64
			if err := ep.acc.GetBuffer(&data, &frames, &flags, &devPos, &qpc); err != nil {
				return nil, s.lost(err)
			}
			got = true
			n := int(frames) * ep.conv.block
			var pcm []byte
			if flags&wca.AUDCLNT_BUFFERFLAGS_SILENT != 0 {
				// A silent packet's data is to be ignored, not read.
				pcm = ep.conv.convert(make([]byte, n))
			} else {
				pcm = ep.conv.convert(unsafe.Slice(data, n))
			}
			ep.acc.ReleaseBuffer(frames)
			// A live stream writes no silence, so it has no use for the clock. Sleep is measured afresh for every packet, because emit can stall long enough for the machine to sleep between two of them.
			if !s.live {
				clock.lead = sleepLead()
				if gap := clock.silenceBefore(int64(qpc), int(frames), ep.conv.from, flags&wca.AUDCLNT_BUFFERFLAGS_TIMESTAMP_ERROR != 0, qpcHNS()); gap > 0 {
					if !send(chunk) || !quiet(gap) {
						return nil, false
					}
					chunk = nil
				}
			}
			chunk = append(chunk, pcm...)
		}
		if !send(chunk) {
			return nil, false
		}
		// Only a poll that found the buffer empty says the device went quiet. One that found packets can still be far behind now, when emit's write stalled or the thread woke late, and the packets for that stretch are waiting in the device buffer; silence written over it would push them in after it and leave the rest of the track running late.
		if !s.live && !got {
			clock.lead = sleepLead()
			if !quiet(clock.idle(qpcHNS())) {
				return nil, false
			}
		}
	}
}

// lost decides what a capture error means. Input: the error from the capture client. Output: true to reopen, for a device that went away and for any error at all in a live stream, which has no better option than trying again; otherwise false, with failed set.
func (s *wasapiStream) lost(err error) bool {
	if s.live || recoverable(err) {
		slog.Warn("audio device lost, waiting for a default device", "error", err)
		return true
	}
	slog.Warn("audio stream failed", "error", err)
	s.failed.Store(true)
	return false
}

// recoverable reports whether err from a WASAPI call says the device went away rather than that the stream itself is broken.
func recoverable(err error) bool {
	var oe *ole.OleError
	if !errors.As(err, &oe) {
		return false
	}
	switch uint32(oe.Code()) {
	case audclntDeviceInvalidated, audclntServiceNotRunning, audclntResourcesInvalidated:
		return true
	}
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
	procUIT  = windows.NewLazySystemDLL("kernel32.dll").NewProc("QueryUnbiasedInterruptTime")
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

// sleepLead is how far the performance counter is ahead of the unbiased interrupt time, in 100 ns units. Both count from boot, but only the performance counter goes on while the machine sleeps or hibernates, so the lead grows by exactly the time slept, which is how packetClock tells a sleep from a device that was merely quiet.
func sleepLead() int64 {
	var awake uint64
	procUIT.Call(uintptr(unsafe.Pointer(&awake)))
	return qpcHNS() - int64(awake)
}
