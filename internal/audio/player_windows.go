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

// number of hours spent here: 4

type otoPlayer struct {
	ctx *oto.Context
	// oto is a low-level os-agnostic audio lib (speaker)
	// we store WCA handles here if later needed for cleanup

	// Windows Core Audio 2006, lowest audio level possible, allows contains a share mode for multi-active-window mic capturing
	player   *oto.Player
	streamer *audioStreamer
}

func NewSpeaker() (Speaker, error) {
	// initialize oto for speaker
	/* llm api infodump
	- gemini live api requires 24kHz Mono 16-bit
	*/
	op := &oto.NewContextOptions{
		SampleRate:   24000,
		ChannelCount: 1,
		Format:       oto.FormatSignedInt16LE,
	}

	ctx, readyChan, err := oto.NewContext(op)
	if err != nil {
		return nil, fmt.Errorf("failed to init oto context: %w", err)
	}
	<-readyChan

	// here we will create streamer with a buffered channel (upto 10000 chunks)
	// i tried 1024, but it got choppy, maybe 10k should work
	streamer := &audioStreamer{
		chunks: make(chan []byte, 10000),
	}

	// now we give it to oto and start it
	player := ctx.NewPlayer(streamer) // this is extremely greedy, so initially eats silence
	player.Play()

	return &otoPlayer{
		ctx:      ctx,
		player:   player,
		streamer: streamer,
	}, nil
}

func (p *otoPlayer) Play(pcm []byte) error {
	// drop the audio chunk here, and read() should pick it up
	// the backpressure is required in streaming media. natural backpressure forces the llm to wait for real time playback?
	// i think im right but i'll see? update:
	// a non-blocking send to ensure the agent never deadlocks if the audio buffer is full as dropping a chunk is better than hanging the whole process ig
	select {
	case p.streamer.chunks <- pcm:
	default:
		// TODO: logger warning here if needed
	}
	return nil
}

func (p *otoPlayer) CurrentAmplitude() float64 {
	return math.Float64frombits(p.streamer.currentAmp.Load())
}

// INTERRUPT HANDLING HAHA
func (p *otoPlayer) Flush() {
	// drain the chunks channel immediately
drain:
	for {
		select {
		case <-p.streamer.chunks:
		default:
			break drain
		}
	}
	// also clear the active buffer in the streamer under lock — Read() runs on a separate OS audio thread
	p.streamer.mu.Lock()
	p.streamer.buffer = nil
	p.streamer.mu.Unlock()
}

func (p *otoPlayer) Close() error {
	// closing the channel here makes it so that Read() knows to return io.EOF
	close(p.streamer.chunks)
	return nil
}

type audioStreamer struct {
	mu         sync.Mutex
	chunks     chan []byte
	buffer     []byte
	currentAmp atomic.Uint64
}

func (s *audioStreamer) Read(p []byte) (n int, err error) {
	s.mu.Lock()
	bufLen := len(s.buffer)
	s.mu.Unlock()

	if bufLen == 0 {
		select {
		case chunk, ok := <-s.chunks:
			if !ok {
				return 0, io.EOF // close the channel + end of stream
			}
			s.mu.Lock()
			s.buffer = chunk
			s.mu.Unlock()
		default:
			// this should run when no audio is ready
			// returning silence for some time so that hardware doesn't deadlock
			// we fill the entire buffer p to maintain clock sync
			for i := range p {
				p[i] = 0
			}
			s.currentAmp.Store(0)
			time.Sleep(time.Millisecond)
			// we dont want cpu spinning and just blocking everything either
			return len(p), nil
		}
	}

	// copy real audio (recoreded) into the destination buffer
	s.mu.Lock()
	n = copy(p, s.buffer)
	s.buffer = s.buffer[n:]
	s.mu.Unlock()

	// rms amplitude scaled up to match perceived sensitivity of peak.
	// Raw rms is ~3x lower than peak for speech; multiply to restore range.
	samples := n / 2
	var sum float64
	for i := 0; i+1 < n; i += 2 {
		s16 := float64(int16(p[i]) | int16(p[i+1])<<8)
		sum += s16 * s16
	}
	var rms float64
	if samples > 0 {
		rms = math.Sqrt(sum/float64(samples)) / 32768.0 * 3.0
		if rms > 1.0 {
			rms = 1.0
		}
	}
	s.currentAmp.Store(math.Float64bits(rms))

	return n, nil
}
