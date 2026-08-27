//go:build linux

package audio

import (
	"fmt"
	"io"
	"time"

	"github.com/jfreymuth/pulse"
	"github.com/jfreymuth/pulse/proto"
)

// MeetingCapture is a pair of live PulseAudio record streams: the default source (the user's microphone) and the default sink's monitor (everything the machine plays, which in a call is everyone else).
// MicStart and SystemStart are the wall-clock instants each stream began, so transcripts of the two files can be lined up on one clock.
type MeetingCapture struct {
	MicStart    time.Time
	SystemStart time.Time

	client  *pulse.Client
	streams []*pulse.RecordStream
}

// StartMeetingCapture opens both streams on their own PulseAudio connection and writes 16 kHz mono s16le samples into mic and system until Stop is called.
// Sharing the microphone with the voice agent needs no special handling: PulseAudio and PipeWire both fan one source out to every record stream attached to it, and this opens its own client rather than borrowing the agent's.
// Input: two writers, one per stream. Output: a running capture, or an error if either stream could not be opened.
func StartMeetingCapture(mic, system io.Writer) (*MeetingCapture, error) {
	client, err := pulse.NewClient(pulse.ClientApplicationName("Ora meeting recorder"))
	if err != nil {
		return nil, fmt.Errorf("connect to pulse: %w", err)
	}
	c := &MeetingCapture{client: client}

	sink, err := client.DefaultSink()
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("find default sink: %w", err)
	}

	micStream, err := c.open(mic, "Ora meeting (microphone)")
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("open microphone stream: %w", err)
	}
	sysStream, err := c.open(system, "Ora meeting (system audio)", pulse.RecordMonitor(sink))
	if err != nil {
		micStream.Close()
		client.Close()
		return nil, fmt.Errorf("open monitor stream for sink %q: %w", sink.ID(), err)
	}

	c.MicStart = time.Now()
	micStream.Start()
	c.SystemStart = time.Now()
	sysStream.Start()
	c.streams = []*pulse.RecordStream{micStream, sysStream}
	return c, nil
}

// open creates one 16 kHz mono record stream that forwards its samples to w as little-endian 16-bit PCM.
func (c *MeetingCapture) open(w io.Writer, name string, extra ...pulse.RecordOption) (*pulse.RecordStream, error) {
	opts := []pulse.RecordOption{
		pulse.RecordSampleRate(sampleRate16k),
		pulse.RecordChannels(proto.ChannelMap{proto.ChannelMono}),
		// RecordLatency is required: PipeWire's pulse server delivers no data to a record stream that leaves buffer attributes unset (fragment/latency).
		pulse.RecordLatency(0.2),
		pulse.RecordMediaName(name),
	}
	opts = append(opts, extra...)
	return c.client.NewRecord(pulse.Int16Writer(func(in []int16) (int, error) {
		if len(in) == 0 {
			return 0, nil
		}
		buf := make([]byte, len(in)*2)
		for i, s := range in {
			buf[i*2] = byte(s)
			buf[i*2+1] = byte(s >> 8)
		}
		if _, err := w.Write(buf); err != nil {
			return 0, err
		}
		return len(in), nil
	}), opts...)
}

// sampleRate16k is what whisper wants, and capturing at it avoids a resample step later.
const sampleRate16k = 16000

// Stop halts both streams and closes the PulseAudio connection. Safe to call once; the caller closes the writers afterwards.
func (c *MeetingCapture) Stop() {
	for _, s := range c.streams {
		s.Stop()
		s.Close()
	}
	c.streams = nil
	c.client.Close()
}
