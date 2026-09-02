//go:build linux

package audio

import (
	"testing"

	"github.com/jfreymuth/pulse/proto"
)

// input builds one sink input as PulseAudio would report it: which sink it plays into, whether it is paused, and which process owns it.
func input(sink uint32, corked bool, pid string) *proto.GetSinkInputInfoReply {
	return &proto.GetSinkInputInfoReply{
		SinkIndex:  sink,
		Corked:     corked,
		Properties: proto.PropList{"application.process.id": proto.PropListString(pid)},
	}
}

// The meeting is recorded from the sink something is actually playing into, not from whatever the default sink happens to be: a call moved to headphones while the default is still the laptop speakers used to record 38 minutes of digital silence.
func TestActiveSinkIndex(t *testing.T) {
	cases := []struct {
		name   string
		inputs []*proto.GetSinkInputInfoReply
		want   uint32
		wantOK bool
	}{
		{
			name:   "nothing is playing, so there is nothing better than the default sink",
			inputs: nil,
		},
		{
			name:   "one application is playing, so record the sink it plays into",
			inputs: []*proto.GetSinkInputInfoReply{input(7, false, "4242")},
			want:   7,
			wantOK: true,
		},
		{
			name:   "a paused stream is not playing anything, so its sink proves nothing",
			inputs: []*proto.GetSinkInputInfoReply{input(7, true, "4242")},
		},
		{
			name:   "Ora's own speech is not the meeting, so its sink must not be picked over a real one",
			inputs: []*proto.GetSinkInputInfoReply{input(1, false, "999"), input(7, false, "4242")},
			want:   7,
			wantOK: true,
		},
		{
			name:   "Ora talking to itself with nothing else playing leaves the default sink alone",
			inputs: []*proto.GetSinkInputInfoReply{input(1, false, "999")},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := activeSinkIndex(c.inputs, "999")
			if ok != c.wantOK {
				t.Fatalf("found = %v, want %v", ok, c.wantOK)
			}
			if ok && got != c.want {
				t.Errorf("sink index = %d, want %d", got, c.want)
			}
		})
	}
}
