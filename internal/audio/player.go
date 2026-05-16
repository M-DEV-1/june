package audio

import (
	"fmt"
	"io"
	"math"
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
	player     *oto.Player
	streamer   *audioStreamer
	currentAmp atomic.Uint64
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
	var maxAmp int16
	for i := 0; i < len(pcm)-1; i += 2 {
		sample := int16(pcm[i]) | int16(pcm[i+1])<<8
		if sample < 0 {
			sample = -sample
		}
		if sample > maxAmp {
			maxAmp = sample
		}
	}
	p.currentAmp.Store(math.Float64bits(float64(maxAmp) / 32768.0))

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
	return math.Float64frombits(p.currentAmp.Load())
}

// INTERRUPT HANDLING HAHA
func (p *otoPlayer) Flush() {
	// drain the chunks channel immediately
	for len(p.streamer.chunks) > 0 {
		select {
		case <-p.streamer.chunks:
		default:
			return
		}
	}
	// also clear the active buffer in the streamer
	p.streamer.buffer = nil
}

func (p *otoPlayer) Close() error {
	// closing the channel here makes it so that Read() knows to return io.EOF
	close(p.streamer.chunks)
	return nil
}

type audioStreamer struct {
	chunks chan []byte
	buffer []byte
}

func (s *audioStreamer) Read(p []byte) (n int, err error) {
	// if no current audio, check channel
	if len(s.buffer) == 0 {
		select {
		case chunk, ok := <-s.chunks:
			if !ok {
				return 0, io.EOF // close the channel + end of stream
			}
			s.buffer = chunk
		default:
			// this should run when no audio is ready
			// returning silence for some time so that hardware doesn't deadlock
			// we fill the entire buffer p to maintain clock sync
			silenceLen := len(p) // 24khz mono 16bit
			for i := 0; i < silenceLen; i++ {
				p[i] = 0
			}
			time.Sleep(time.Millisecond)
			// we dont want cpu spinning and just blocking everything either
			return silenceLen, nil
		}
	}

	// copy real audio (recoreded) into the destination buffer

	n = copy(p, s.buffer)
	s.buffer = s.buffer[n:]
	return n, nil
}
