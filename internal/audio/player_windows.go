//go:build windows

package audio

import (
	"fmt"
	"io"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ebitengine/oto/v3"
)

// oto allows one context per process, while June opens a speaker per voice session and per voice preview, so the context is made once and each speaker is a player on it.
var (
	otoOnce sync.Once
	otoCtx  *oto.Context
	otoErr  error
)

// otoContext returns the process's oto context at 24 kHz mono s16le, the same format the Linux speaker plays.
func otoContext() (*oto.Context, error) {
	otoOnce.Do(func() {
		ctx, ready, err := oto.NewContext(&oto.NewContextOptions{
			SampleRate:   24000,
			ChannelCount: 1,
			Format:       oto.FormatSignedInt16LE,
		})
		if err != nil {
			otoErr = fmt.Errorf("open audio output: %w", err)
			return
		}
		<-ready
		otoCtx = ctx
	})
	return otoCtx, otoErr
}

// otoPlayerBuffer is how much audio oto holds ahead of the device: 100 ms. oto's default is half a second, and since the streamer fills idle time with silence, every reply would start that late.
const otoPlayerBuffer = 24000 * 2 / 10

type otoPlayer struct {
	player   *oto.Player
	streamer *audioStreamer
}

func NewSpeaker() (Speaker, error) {
	ctx, err := otoContext()
	if err != nil {
		return nil, err
	}
	streamer := &audioStreamer{chunks: make(chan []byte, 10000)}
	player := ctx.NewPlayer(streamer)
	player.SetBufferSize(otoPlayerBuffer)
	player.Play()
	return &otoPlayer{player: player, streamer: streamer}, nil
}

// Play queues pcm (24 kHz mono s16le) without blocking; a chunk that does not fit is dropped.
func (p *otoPlayer) Play(pcm []byte) error {
	select {
	case p.streamer.chunks <- pcm:
	default:
	}
	return nil
}

func (p *otoPlayer) CurrentAmplitude() float64 {
	return math.Float64frombits(p.streamer.currentAmp.Load())
}

// Flush drops everything queued: the chunks not yet read, the streamer's partial chunk, and what oto has already pulled into its own buffer.
func (p *otoPlayer) Flush() {
drain:
	for {
		select {
		case <-p.streamer.chunks:
		default:
			break drain
		}
	}
	p.streamer.mu.Lock()
	p.streamer.buffer = nil
	p.streamer.mu.Unlock()
	// Seek is how oto clears a player's buffer and keeps it playing; audioStreamer accepts it as a no-op.
	p.player.Seek(0, io.SeekStart)
}

// Close silences the speaker. The player stays registered with oto until it is garbage collected, which is how oto v3 releases players.
func (p *otoPlayer) Close() error {
	p.player.Pause()
	p.Flush()
	return nil
}

// audioStreamer is the io.Reader oto pulls from: queued chunks first, silence when none are waiting, so the device clock never stalls.
type audioStreamer struct {
	mu         sync.Mutex
	chunks     chan []byte
	buffer     []byte
	currentAmp atomic.Uint64
}

func (s *audioStreamer) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buffer) == 0 {
		select {
		case s.buffer = <-s.chunks:
		default:
			clear(p)
			s.currentAmp.Store(0)
			// oto calls Read in a loop; the pause keeps an idle speaker from spinning a core.
			time.Sleep(time.Millisecond)
			return len(p), nil
		}
	}
	n := copy(p, s.buffer)
	s.buffer = s.buffer[n:]
	s.currentAmp.Store(math.Float64bits(level(p[:n])))
	return n, nil
}

// Seek lets oto's Player.Seek clear the player's buffer; there is no position to move, so it does nothing.
func (s *audioStreamer) Seek(int64, int) (int64, error) {
	return 0, nil
}
