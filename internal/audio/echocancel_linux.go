//go:build linux

package audio

import (
	"log/slog"
	"sync"
	"time"

	"github.com/jfreymuth/pulse"
	"github.com/jfreymuth/pulse/proto"
)

// echoCancelArgs are the arguments for the audio server's echo-cancel module: the WebRTC canceller with noise suppression and the extended filter on and automatic gain control off, which pulled the source's volume to near zero, creating EchoCancelSource and EchoCancelSink.
const echoCancelArgs = "source_name=" + EchoCancelSource + " sink_name=" + EchoCancelSink + ` aec_method=webrtc aec_args="noise_suppression=1 extended_filter=1 analog_gain_control=0 digital_gain_control=0"`

// echoCancelWait is how long a freshly loaded canceller is given to show its source and sink, and echoCancelPoll how often they are looked for.
const (
	echoCancelWait = 3 * time.Second
	echoCancelPoll = 50 * time.Millisecond
)

// audioServer is the part of the audio server connection the echo canceller uses. *pulse.Client is one; the tests answer for a fake.
type audioServer interface {
	SourceByID(name string) (*pulse.Source, error)
	SinkByID(name string) (*pulse.Sink, error)
	RawRequest(req proto.RequestArgs, rpl proto.Reply) error
}

// startEchoCancel makes sure the audio server has the echo-cancelled source and sink before the voice session opens its microphone and speaker. Output: a function that unloads the canceller when this call is what loaded it, and does nothing otherwise. It is safe to call more than once, and it is never nil: a server that cannot be reached or will not load the module leaves the session on the default microphone and speaker.
func startEchoCancel() func() {
	c, err := pulse.NewClient(pulse.ClientApplicationName("June voice"))
	if err != nil {
		slog.Warn("could not reach the audio server to set up echo cancellation", "error", err)
		return func() {}
	}
	unload := ensureEchoCancel(c, echoCancelWait)
	var once sync.Once
	return func() {
		once.Do(func() {
			unload()
			c.Close()
		})
	}
}

// ensureEchoCancel loads the echo canceller unless the server already has its source and sink, and waits up to wait for them to appear. Output: a function that unloads the module if this call loaded it, and does nothing if the source and sink were there already or the load was refused.
func ensureEchoCancel(c audioServer, wait time.Duration) func() {
	if echoCancelPresent(c) {
		slog.Info("echo canceller already loaded, leaving it", "source", EchoCancelSource, "sink", EchoCancelSink)
		return func() {}
	}
	var loaded proto.LoadModuleReply
	if err := c.RawRequest(&proto.LoadModule{Name: "module-echo-cancel", Args: echoCancelArgs}, &loaded); err != nil {
		slog.Warn("could not load the echo canceller; June listens on the default microphone and may hear its own voice", "error", err)
		return func() {}
	}
	for deadline := time.Now().Add(wait); !echoCancelPresent(c) && time.Now().Before(deadline); {
		time.Sleep(echoCancelPoll)
	}
	if echoCancelPresent(c) {
		slog.Info("loaded the echo canceller for this voice session", "source", EchoCancelSource, "sink", EchoCancelSink, "module", loaded.ModuleIndex)
	} else {
		slog.Warn("loaded the echo canceller but its source and sink did not appear; June may listen on the default microphone", "module", loaded.ModuleIndex)
	}
	return func() {
		if err := c.RawRequest(&proto.UnloadModule{ModuleIndex: loaded.ModuleIndex}, nil); err != nil {
			slog.Warn("could not unload the echo canceller", "module", loaded.ModuleIndex, "error", err)
		}
	}
}

// echoCancelPresent reports whether the server has both the echo-cancelled source and sink.
func echoCancelPresent(c audioServer) bool {
	_, srcErr := c.SourceByID(EchoCancelSource)
	_, sinkErr := c.SinkByID(EchoCancelSink)
	return srcErr == nil && sinkErr == nil
}
