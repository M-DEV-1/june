//go:build linux

package audio

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
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

	sink, err := meetingSink(client)
	if err != nil {
		client.Close()
		return nil, err
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

	// ponytail: these two instants are when Start() returned on this side of the PulseAudio round-trip, not when the server actually began handing over samples, so the mic/system offset they give the transcript is good to roughly a stream latency (0.2s here) rather than to the sample. Worse, an underrun drops samples mid-stream, so the two transcripts drift further apart the longer a meeting runs and no offset measured at the start can correct it. Fixing it properly means timestamping from the stream's own sample counter and reading the server's underrun reports; do that if long meetings come out visibly misaligned.
	c.MicStart = time.Now()
	micStream.Start()
	c.SystemStart = time.Now()
	sysStream.Start()
	c.streams = []*pulse.RecordStream{micStream, sysStream}
	return c, nil
}

// meetingSink returns the sink whose monitor should be recorded: the one an application is actually playing into, falling back to the default sink when nothing is playing yet or when the server will not say.
// The default sink alone is not enough. Moving a call to headphones or a dock changes which sink it plays into without necessarily changing the default, and monitoring the wrong sink records a file of pure silence that looks like a working recording until the transcript comes back empty.
func meetingSink(client *pulse.Client) (*pulse.Sink, error) {
	def, err := client.DefaultSink()
	if err != nil {
		return nil, fmt.Errorf("find default sink: %w", err)
	}
	var inputs proto.GetSinkInputInfoListReply
	if err := client.RawRequest(&proto.GetSinkInputInfoList{}, &inputs); err != nil {
		slog.Warn("could not list what is playing, recording the default sink", "error", err)
		return def, nil
	}
	index, ok := activeSinkIndex(inputs, strconv.Itoa(os.Getpid()))
	if !ok || index == def.SinkIndex() {
		return def, nil
	}
	sinks, err := client.ListSinks()
	if err != nil {
		slog.Warn("could not list sinks, recording the default sink", "error", err)
		return def, nil
	}
	for _, s := range sinks {
		if s.SinkIndex() == index {
			slog.Info("recording the sink something is playing into rather than the default sink", "sink", s.ID(), "default", def.ID())
			return s, nil
		}
	}
	return def, nil
}

// activeSinkIndex returns the sink index of the first stream that is actually playing, skipping paused streams and Ora's own speech (which is not part of the meeting and would drag the recording back to whatever sink the assistant talks through).
// Input: the sink inputs PulseAudio reports, and this process's PID as a string. Output: the sink index and true, or false when nothing else is playing.
func activeSinkIndex(inputs []*proto.GetSinkInputInfoReply, ownPID string) (uint32, bool) {
	for _, in := range inputs {
		if in.Corked || in.Properties["application.process.id"].String() == ownPID {
			continue
		}
		return in.SinkIndex, true
	}
	return 0, false
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
