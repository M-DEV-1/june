//go:build linux

package audio

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jfreymuth/pulse"
	"github.com/jfreymuth/pulse/proto"
)

// fakeServer answers for an audio server: the echo canceller's source and sink exist once present is set, which a load request does after appearAfter polls, or never when appearAfter is negative.
type fakeServer struct {
	present     bool
	loadErr     error
	appearAfter int
	loads       []string
	unloads     []uint32
	lookups     int
}

func (f *fakeServer) SourceByID(string) (*pulse.Source, error) {
	f.lookups++
	if f.appearAfter > 0 && f.lookups > f.appearAfter {
		f.present = true
	}
	if !f.present {
		return nil, errors.New("no such source")
	}
	return &pulse.Source{}, nil
}

func (f *fakeServer) SinkByID(string) (*pulse.Sink, error) {
	if !f.present {
		return nil, errors.New("no such sink")
	}
	return &pulse.Sink{}, nil
}

func (f *fakeServer) RawRequest(req proto.RequestArgs, rpl proto.Reply) error {
	switch r := req.(type) {
	case *proto.LoadModule:
		if f.loadErr != nil {
			return f.loadErr
		}
		f.loads = append(f.loads, r.Name+" "+r.Args)
		rpl.(*proto.LoadModuleReply).ModuleIndex = 42
		if f.appearAfter == 0 {
			f.present = true
		}
	case *proto.UnloadModule:
		f.unloads = append(f.unloads, r.ModuleIndex)
		f.present = false
	}
	return nil
}

func TestEnsureEchoCancel_LeavesAnExistingCancellerAlone(t *testing.T) {
	f := &fakeServer{present: true}
	ensureEchoCancel(f, time.Second)()
	if len(f.loads) != 0 || len(f.unloads) != 0 {
		t.Fatalf("loads %v, unloads %v; a canceller that was already there is neither loaded nor unloaded", f.loads, f.unloads)
	}
}

func TestEnsureEchoCancel_LoadsUnderJunesNamesAndUnloadsWhatItLoaded(t *testing.T) {
	f := &fakeServer{}
	release := ensureEchoCancel(f, time.Second)
	if len(f.loads) != 1 || !strings.HasPrefix(f.loads[0], "module-echo-cancel ") || !strings.Contains(f.loads[0], "source_name="+EchoCancelSource+" ") || !strings.Contains(f.loads[0], "sink_name="+EchoCancelSink+" ") {
		t.Fatalf("loads = %v, want one module-echo-cancel naming %s and %s", f.loads, EchoCancelSource, EchoCancelSink)
	}
	if len(f.unloads) != 0 {
		t.Fatalf("unloaded %v before the session ended", f.unloads)
	}
	release()
	if len(f.unloads) != 1 || f.unloads[0] != 42 {
		t.Fatalf("unloads = %v, want the module the load returned (42)", f.unloads)
	}
}

func TestEnsureEchoCancel_WaitsForTheSourceAndSinkToAppear(t *testing.T) {
	f := &fakeServer{appearAfter: 3}
	ensureEchoCancel(f, time.Second)
	if !f.present {
		t.Fatal("returned before the source and sink appeared")
	}
}

func TestEnsureEchoCancel_RefusedLoadUnloadsNothing(t *testing.T) {
	f := &fakeServer{loadErr: errors.New("no such module")}
	ensureEchoCancel(f, time.Second)()
	if len(f.unloads) != 0 {
		t.Fatalf("unloads = %v after a refused load", f.unloads)
	}
}

func TestEnsureEchoCancel_UnloadsAModuleWhoseNodesNeverShowed(t *testing.T) {
	f := &fakeServer{appearAfter: -1}
	ensureEchoCancel(f, 20*time.Millisecond)()
	if len(f.unloads) != 1 {
		t.Fatalf("unloads = %v, want the module this call loaded to go", f.unloads)
	}
}
