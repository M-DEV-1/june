//go:build windows

package audio

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
)

// speakerRate is what June plays: 24 kHz mono s16le, the format Gemini speaks in and the Linux speaker plays.
const speakerRate = 24000

// renderBuffer is the device buffer asked for, 100 ms in 100 ns units. Audio is written ahead of the device by at most this much, which is all a Flush has to throw away at the device, and it is ten polls deep, so a late wake-up does not run it dry.
const renderBuffer = 1_000_000

// renderPoll is how often the render thread tops the device buffer up and reads back how far playback has got, which is also how fresh CurrentAmplitude is.
const renderPoll = 10 * time.Millisecond

// levelFrames is how much audio one amplitude reading covers, 20 ms: the resolution the echo gate and the waveform see June's voice at.
const levelFrames = speakerRate / 50

// outputLatency is how long a sample takes to sound once the device clock has counted it played. The clock counts to where the audio engine took it from the stream, not to the speaker: measured 2026-10-03 on this machine's speakers, the end of a tone reached the endpoint's loopback 44 ms after the clock had passed it, and the device's own buffer still lies beyond that point. The last level is held this long after the clock runs out, and Drain waits it out too.
const outputLatency = 80 * time.Millisecond

// stallPolls is how many polls in a row the device clock may stand still, with everything written already taken from the buffer, before the audio counts as played out. Most devices' clocks land exactly on what was written; one that stops a frame short would otherwise read as playing for ever.
const stallPolls = 5

// errSpeakerClosed is what Play and Drain report once the speaker is closed.
var errSpeakerClosed = errors.New("speaker is closed")

// The voice preview finds Drain by a type assertion, so a Drain that drifts from Drainer would compile and leave the preview cut off; this makes it fail to build instead.
var _ Drainer = (*winSpeaker)(nil)

// winSpeaker plays through the default console playback device with WASAPI in shared mode, on its own render thread (see onCOMThread), and follows the default device the way the capture streams do.
// It is not oto, which allows one audio context per process: the context it found first, a failed one or one with no device behind it included, was the daemon's for life, and every later speaker played into it without a word. Each speaker here opens the device afresh and says when it cannot.
// CurrentAmplitude is what the device is playing at this moment, read back from the device clock and held for outputLatency past it, not what was last handed over. Windows gets no echo canceller, so the echo gate and the wait for June to go quiet have to hold until her last word has actually left the speaker; oto's buffers put the level a good 150 ms ahead of the sound.
type winSpeaker struct {
	chunks     chan []byte
	currentAmp atomic.Uint64
	stop       chan struct{}
	done       chan struct{}
	once       sync.Once

	mu sync.Mutex
	// reset is set by Flush for the render thread, which has to empty the device buffer as well.
	reset bool
	// idle is true when everything the render thread has taken off chunks has been heard.
	idle bool
	// err is why there is nothing to play to right now: no device, or the speaker is closed. nil while there is a device.
	err error
}

// renderEndpoint is one opened and started playback device.
type renderEndpoint struct {
	id  string
	ac  *wca.IAudioClient
	arc *wca.IAudioRenderClient
	// clock reads back how far playback has got, with freq its units per second; nil when the device offers none, and then the buffer's padding stands in for it.
	clock *audioClock
	freq  uint64
	// frames is the size of the device buffer.
	frames uint32
	out    *outConverter
}

func (e *renderEndpoint) close() {
	e.ac.Stop()
	if e.clock != nil {
		e.clock.Release()
	}
	e.arc.Release()
	e.ac.Release()
}

// player is the render thread's own state: the stream it writes to and how far playback has got. No other goroutine touches it.
type player struct {
	ep *renderEndpoint
	// rest is what is left of the chunk being played, not yet converted.
	rest []byte
	// cur is the converted block being written, and level how loud it is.
	cur   []byte
	level float64
	// written counts the device frames written since the stream started or was last reset, and marks how loud each stretch of them is, oldest first, back to the first one not yet heard.
	written int64
	marks   []mark
	// tail is the level of the last stretch the clock passed, at heardAt, which is still sounding for outputLatency after.
	tail    float64
	heardAt time.Time
	// base is how far the device clock ran past what was written while it had nothing to play, for a device whose clock keeps going through silence.
	base int64
	// last is the previous reading of the device clock, and stalls how many polls in a row it has stood at that reading with nothing left in the buffer.
	last   int64
	stalls int
	// out is the scratch the next write is gathered in.
	out []byte
}

// mark is the end of one stretch of written audio, in device frames, and how loud that stretch is.
type mark struct {
	end   int64
	level float64
}

// NewSpeaker opens the default playback device. Input: none. Output: the speaker, or the error that kept the device from opening, in which case the next call tries the device again.
func NewSpeaker() (Speaker, error) {
	s := &winSpeaker{chunks: make(chan []byte, 10000), stop: make(chan struct{}), done: make(chan struct{}), idle: true}
	err := onCOMThread(s.done, func(de *wca.IMMDeviceEnumerator, started func(error)) {
		ep, err := openRender(de)
		started(err)
		if err == nil {
			s.run(de, ep)
		}
	})
	if err != nil {
		return nil, fmt.Errorf("open audio output: %w", err)
	}
	return s, nil
}

// openRender activates the default console playback device, initialises it in shared mode and starts it. The console device is the one a browser, a media player and Windows' own sounds use. Like openWASAPI it first asks Windows to take June's format itself and falls back to the device's mix format, converted in Go.
// Input: the device enumerator. Output: the started endpoint, or an error naming the step that failed.
func openRender(de *wca.IMMDeviceEnumerator) (*renderEndpoint, error) {
	var out *outConverter
	id, ac, err := activateDefault(de, wca.ERender, wca.EConsole, func(ac *wca.IAudioClient, direct bool) (err error) {
		out, err = initRender(ac, direct)
		return err
	})
	if err != nil {
		return nil, err
	}
	e := &renderEndpoint{id: id, ac: ac, out: out}
	if err := ac.GetBufferSize(&e.frames); err != nil {
		ac.Release()
		return nil, fmt.Errorf("read buffer size: %w", err)
	}
	if err := ac.GetService(wca.IID_IAudioRenderClient, &e.arc); err != nil {
		ac.Release()
		return nil, fmt.Errorf("get render client: %w", err)
	}
	var clock *audioClock
	if ac.GetService(wca.IID_IAudioClock, &clock) == nil && clock != nil {
		if freq, err := clock.frequency(); err == nil && freq > 0 {
			e.clock, e.freq = clock, freq
		} else {
			clock.Release()
		}
	}
	if err := ac.Start(); err != nil {
		if e.clock != nil {
			e.clock.Release()
		}
		e.arc.Release()
		ac.Release()
		return nil, fmt.Errorf("start audio client: %w", err)
	}
	return e, nil
}

// initRender initialises ac for playback in shared mode. Direct asks for speakerRate mono 16-bit with Windows' own converter; otherwise the device's mix format is used as is and the audio is converted to it in Go.
// Input: the client and which attempt this is. Output: the converter from June's audio to what this client takes.
func initRender(ac *wca.IAudioClient, direct bool) (*outConverter, error) {
	if direct {
		flags := uint32(wca.AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM | wca.AUDCLNT_STREAMFLAGS_SRC_DEFAULT_QUALITY)
		if err := ac.Initialize(wca.AUDCLNT_SHAREMODE_SHARED, flags, renderBuffer, 0, monoFormat(speakerRate), nil); err != nil {
			return nil, err
		}
		return &outConverter{block: 2, rate: speakerRate, direct: true}, nil
	}
	mix, float, err := mixFormat(ac)
	if err != nil {
		return nil, err
	}
	defer ole.CoTaskMemFree(uintptr(unsafe.Pointer(mix)))
	if err := ac.Initialize(wca.AUDCLNT_SHAREMODE_SHARED, 0, renderBuffer, 0, mix, nil); err != nil {
		return nil, err
	}
	return &outConverter{
		block:    int(mix.NBlockAlign),
		rate:     int(mix.NSamplesPerSec),
		channels: int(mix.NChannels),
		float:    float,
		rs:       resampler{from: speakerRate, to: int(mix.NSamplesPerSec)},
	}, nil
}

// outConverter turns June's 16-bit mono PCM into what one playback device takes: the other direction from converter.
type outConverter struct {
	// block is the size of one device frame in bytes.
	block int
	// rate is the device's frame rate.
	rate int
	// direct is set when the device takes June's audio as it is and Windows does the converting.
	direct   bool
	channels int
	float    bool
	rs       resampler
}

// convert returns pcm in the device's format. Input: 16-bit little-endian mono PCM. Output: whole device frames, the same mono sample in every channel; pcm itself when the device takes it as is.
func (c *outConverter) convert(pcm []byte) []byte {
	if c.direct {
		return pcm
	}
	in := make([]float64, len(pcm)/2)
	for i := range in {
		in[i] = float64(int16(binary.LittleEndian.Uint16(pcm[i*2:])))
	}
	samples := c.rs.push(in)
	size := c.block / c.channels
	out := make([]byte, len(samples)*c.block)
	for f, v := range samples {
		for ch := 0; ch < c.channels; ch++ {
			b := out[(f*c.channels+ch)*size:]
			if c.float {
				binary.LittleEndian.PutUint32(b, math.Float32bits(float32(v/32768)))
			} else {
				binary.LittleEndian.PutUint16(b, uint16(int16(math.Round(v))))
			}
		}
	}
	return out
}

// run is the render thread: every renderPoll it handles a Flush, publishes how loud the audio playing right now is, and tops the device buffer up from chunks, until Close.
// When the device goes away it waits for a default device for as long as it takes, dropping what was queued, since a reply that turns up seconds late is worse than one lost; when another device becomes the default it moves there, opening the new one before letting the old one go.
func (s *winSpeaker) run(de *wca.IMMDeviceEnumerator, ep *renderEndpoint) {
	p := &player{ep: ep}
	defer func() {
		if p.ep != nil {
			p.ep.close()
		}
	}()
	tick := time.NewTicker(renderPoll)
	defer tick.Stop()
	checked := time.Now()
	var refused string
	for {
		select {
		case <-s.stop:
			return
		case <-tick.C:
		}
		if time.Since(checked) >= deviceCheck {
			checked = time.Now()
			if id := defaultID(de, wca.ERender, wca.EConsole); id != "" && id != p.ep.id {
				next, err := openRender(de)
				switch {
				case err == nil && next.id != p.ep.id:
					slog.Info("speaker moved to the new default device", "device", next.id)
					p.ep.close()
					p.restart(next)
				case err == nil:
					next.close()
				case id != refused:
					slog.Warn("the new default playback device would not open, staying on the current one", "device", id, "error", err)
					refused = id
				}
			}
		}
		if err := s.fill(p); err != nil {
			slog.Warn("speaker lost its device, waiting for a default device", "error", err)
			p.ep.close()
			p.ep = nil
			s.lose(fmt.Errorf("no audio output device: %w", err))
			var next *renderEndpoint
			if retry(s.stop, 0, func() (err error) {
				next, err = openRender(de)
				return err
			}) != nil {
				return
			}
			slog.Info("speaker moved to the current default device", "device", next.id)
			*p = player{ep: next}
			s.mu.Lock()
			s.err = nil
			s.mu.Unlock()
		}
	}
}

// restart puts the player on a newly opened stream, whose clock counts from zero again. The block being written was converted for the old device and goes with it.
func (p *player) restart(ep *renderEndpoint) {
	*p = player{ep: ep, rest: p.rest}
}

// fill handles a pending Flush, publishes the level of what is playing now, and writes as much queued audio as the device buffer has room for. Input: the player. Output: the error from a device call, which means the device is gone.
func (s *winSpeaker) fill(p *player) error {
	ep := p.ep
	s.mu.Lock()
	reset := s.reset
	s.reset = false
	s.mu.Unlock()
	if reset {
		// Stop, Reset and Start are how WASAPI throws away what a stream has buffered; Reset also sets the stream's clock back to zero.
		if err := ep.ac.Stop(); err != nil {
			return err
		}
		if err := ep.ac.Reset(); err != nil {
			return err
		}
		if err := ep.ac.Start(); err != nil {
			return err
		}
		*p = player{ep: ep, out: p.out}
	}
	var padding uint32
	if err := ep.ac.GetCurrentPadding(&padding); err != nil {
		return err
	}

	heard := p.heard(padding)
	i := 0
	for i < len(p.marks) && p.marks[i].end <= heard {
		p.tail, p.heardAt = p.marks[i].level, time.Now()
		i++
	}
	p.marks = p.marks[i:]
	sounding := time.Since(p.heardAt) < outputLatency
	amp := 0.0
	switch {
	case len(p.marks) > 0:
		amp = p.marks[0].level
	case sounding:
		amp = p.tail
	}
	s.currentAmp.Store(math.Float64bits(amp))

	room := int(ep.frames) - int(padding)
	out := p.out[:0]
	at := p.written
	for room > 0 && (len(p.cur) > 0 || s.take(p)) {
		n := min(room, len(p.cur)/ep.out.block)
		if n == 0 {
			p.cur = nil
			continue
		}
		out = append(out, p.cur[:n*ep.out.block]...)
		p.cur = p.cur[n*ep.out.block:]
		at += int64(n)
		p.marks = append(p.marks, mark{end: at, level: p.level})
		room -= n
	}
	p.out = out
	if n := at - p.written; n > 0 {
		var data *byte
		if err := ep.arc.GetBuffer(uint32(n), &data); err != nil {
			return err
		}
		copy(unsafe.Slice(data, len(out)), out)
		if err := ep.arc.ReleaseBuffer(uint32(n), 0); err != nil {
			return err
		}
		p.written = at
	}

	s.mu.Lock()
	s.idle = len(p.marks) == 0 && len(p.cur) == 0 && len(p.rest) < 2 && !sounding
	s.mu.Unlock()
	return nil
}

// heard returns how many of the frames written have played out of the device. Input: the buffer's padding, just read. Output: a count no larger than written.
// The device clock says which sample is at the speaker now. Without one, what has left the buffer is the best there is.
func (p *player) heard(padding uint32) int64 {
	at := p.written - int64(padding)
	if p.ep.clock != nil {
		if pos, err := p.ep.clock.position(); err == nil {
			at = int64(pos*uint64(p.ep.out.rate)/p.ep.freq) - p.base
		}
	}
	if padding > 0 || at != p.last {
		p.stalls = 0
	} else {
		p.stalls++
	}
	p.last = at
	if padding > 0 || (at < p.written && p.stalls < stallPolls) {
		return max(min(at, p.written), 0)
	}
	// Everything written has left the buffer and the clock has reached it, or stood still short of it: the device has finished. A clock that ran on past it was counting silence, which is taken off every later reading so it is not counted as June's audio.
	if at > p.written {
		p.base += at - p.written
		p.last = p.written
	}
	return p.written
}

// take moves the next block of queued audio, at most levelFrames, into p.cur, converted for the device, taking a new chunk off the queue when the last one is used up. Input: the player. Output: false when nothing is queued, or when a Flush is waiting, so audio queued after the Flush is not thrown away with what came before it.
func (s *winSpeaker) take(p *player) bool {
	if len(p.rest) < 2 {
		s.mu.Lock()
		if s.reset {
			s.mu.Unlock()
			return false
		}
		select {
		case p.rest = <-s.chunks:
			s.idle = false
		default:
			p.rest = nil
		}
		s.mu.Unlock()
		if len(p.rest) < 2 {
			return false
		}
	}
	n := min(len(p.rest), 2*levelFrames) &^ 1
	src := p.rest[:n]
	p.rest = p.rest[n:]
	p.cur, p.level = p.ep.out.convert(src), level(src)
	return true
}

// lose publishes that the speaker has no device: until one comes back Play and Drain report err, and whatever was queued is dropped.
func (s *winSpeaker) lose(err error) {
	s.mu.Lock()
	s.err = err
	s.idle = true
	s.reset = false
	discard(s.chunks)
	s.mu.Unlock()
	s.currentAmp.Store(0)
}

// Play queues pcm (24 kHz mono s16le) without blocking; a chunk that does not fit is dropped. Output: nil, or why there is nothing to play it on, in which case it is not queued.
func (s *winSpeaker) Play(pcm []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	select {
	case s.chunks <- pcm:
	default:
	}
	return nil
}

func (s *winSpeaker) CurrentAmplitude() float64 {
	return math.Float64frombits(s.currentAmp.Load())
}

// Flush drops everything queued, for a barge-in. What already sits in the device buffer goes on the render thread's next poll, within renderPoll.
func (s *winSpeaker) Flush() {
	s.mu.Lock()
	discard(s.chunks)
	s.reset = true
	s.mu.Unlock()
}

// Drain waits until everything queued has played out of the device. Input: a context bounding the wait. Output: nil once it has all been heard; otherwise ctx's error, or why the speaker cannot play it.
func (s *winSpeaker) Drain(ctx context.Context) error {
	tick := time.NewTicker(renderPoll)
	defer tick.Stop()
	for {
		s.mu.Lock()
		err, done := s.err, s.idle && !s.reset && len(s.chunks) == 0
		s.mu.Unlock()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// Close stops the render thread and releases the device; whatever was still queued is not played.
func (s *winSpeaker) Close() error {
	s.once.Do(func() { close(s.stop) })
	<-s.done
	s.mu.Lock()
	s.err = errSpeakerClosed
	s.mu.Unlock()
	s.currentAmp.Store(0)
	return nil
}

// discard empties ch without blocking.
func discard(ch chan []byte) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// audioClock is IAudioClock, which go-wca has the IID of but does not wrap.
type audioClock struct {
	ole.IUnknown
}

type audioClockVtbl struct {
	ole.IUnknownVtbl
	GetFrequency       uintptr
	GetPosition        uintptr
	GetCharacteristics uintptr
}

func (c *audioClock) vtable() *audioClockVtbl {
	return (*audioClockVtbl)(unsafe.Pointer(c.RawVTable))
}

// frequency returns the clock's units per second, which is what turns a position into time.
func (c *audioClock) frequency() (uint64, error) {
	var f uint64
	if hr, _, _ := syscall.SyscallN(c.vtable().GetFrequency, uintptr(unsafe.Pointer(c)), uintptr(unsafe.Pointer(&f))); hr != 0 {
		return 0, ole.NewError(hr)
	}
	return f, nil
}

// position returns how far the device has played since the stream started or was last reset, in the clock's units: the sample at the speaker now, not the one last handed to the engine.
func (c *audioClock) position() (uint64, error) {
	var pos uint64
	// S_FALSE (1) is a reading that took long enough to be a little less exact, which is still a reading.
	if hr, _, _ := syscall.SyscallN(c.vtable().GetPosition, uintptr(unsafe.Pointer(c)), uintptr(unsafe.Pointer(&pos)), 0); hr != 0 && hr != 1 {
		return 0, ole.NewError(hr)
	}
	return pos, nil
}
