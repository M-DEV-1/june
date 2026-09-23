package ipc

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ora/internal/recorder"
)

// fakeMic stands in for the machine's microphone: it hands the capture goroutine a fixed list of PCM chunks and then waits for the context to be cancelled, the same shape audio.Microphone has.
type fakeMic struct {
	chunks [][]byte

	mu     sync.Mutex
	closed bool
	opened int
}

func (f *fakeMic) StartCapture(ctx context.Context) (<-chan []byte, error) {
	f.mu.Lock()
	f.opened++
	f.mu.Unlock()
	ch := make(chan []byte, len(f.chunks)+1)
	for _, c := range f.chunks {
		ch <- c
	}
	// The chunks are buffered, so closing on cancel still lets the reader drain every one of them before it sees the channel end.
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

func (f *fakeMic) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeMic) wasClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// fakeTranscriber records the WAV it was handed (its bytes, before the file is deleted) and returns canned text.
type fakeTranscriber struct {
	text string
	err  error

	mu       sync.Mutex
	wav      []byte
	prompt   string
	path     string
	calls    int
	deadline time.Time
}

func (f *fakeTranscriber) run(ctx context.Context, wavPath, prompt string) (string, error) {
	b, _ := os.ReadFile(wavPath)
	f.mu.Lock()
	f.deadline, _ = ctx.Deadline()
	f.wav = b
	f.prompt = prompt
	f.path = wavPath
	f.calls++
	f.mu.Unlock()
	return f.text, f.err
}

func (f *fakeTranscriber) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// newDictationTest wires a Dictation over a fake microphone and a fake transcriber behind a real HTTP server, plus a subscription to the hub the routes broadcast on. mic takes micSource rather than *fakeMic so a test can hand it a different double, such as pausingDictateMic, when it needs to control timing the plain fakeMic cannot.
func newDictationTest(t *testing.T, mic micSource, tx *fakeTranscriber) (*Dictation, *httptest.Server, chan Event) {
	t.Helper()
	s := New(nil, nil, nil, nil)
	d := NewDictation(s)
	d.openMic = func() (micSource, error) { return mic, nil }
	d.transcribe = tx.run
	d.prompt = func(ctx context.Context) string { return "People: Zemna Braxen." }
	d.rate = dictateRate

	mux := http.NewServeMux()
	mux.HandleFunc("/dictate/start", d.Start)
	mux.HandleFunc("/dictate/stop", d.Stop)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	events := s.hub.subscribe()
	t.Cleanup(func() { s.hub.unsubscribe(events) })
	return d, srv, events
}

func startDictation(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	resp, err := http.Post(srv.URL+"/dictate/start", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /dictate/start: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start status = %d, want 202", resp.StatusCode)
	}
	var body struct{ ID string }
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode start body: %v", err)
	}
	if body.ID == "" {
		t.Fatal("start returned an empty id")
	}
	return body.ID
}

func stopDictation(t *testing.T, srv *httptest.Server, id string) *http.Response {
	t.Helper()
	resp, err := http.Post(srv.URL+"/dictate/stop", "application/json", strings.NewReader(`{"id":"`+id+`"}`))
	if err != nil {
		t.Fatalf("POST /dictate/stop: %v", err)
	}
	return resp
}

// pcm builds n samples of 16-bit little-endian audio, each one a different value so a resample or a truncation shows up.
func pcm(n int) []byte {
	b := make([]byte, n*2)
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(int16(i%1000)))
	}
	return b
}

func TestDictationStartStopReturnsTranscript(t *testing.T) {
	mic := &fakeMic{chunks: [][]byte{pcm(1600), pcm(1600)}}
	tx := &fakeTranscriber{text: "  hello there, this is the whole thought chain.  "}
	_, srv, events := newDictationTest(t, mic, tx)

	id := startDictation(t, srv)
	resp := stopDictation(t, srv, id)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stop status = %d, want 200", resp.StatusCode)
	}
	var body struct{ Text string }
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode stop body: %v", err)
	}
	if body.Text != "hello there, this is the whole thought chain." {
		t.Fatalf("text = %q, want the transcriber's text trimmed", body.Text)
	}
	if tx.prompt != "People: Zemna Braxen." {
		t.Fatalf("prompt = %q, want the priming prompt", tx.prompt)
	}
	if !mic.wasClosed() {
		t.Error("the microphone was left open after stop")
	}
	if _, err := os.Stat(tx.path); !os.IsNotExist(err) {
		t.Errorf("the temp WAV %s was not deleted", tx.path)
	}

	ev := mustEvent(t, events)
	if ev.ID != id || ev.Type != "dictation" || ev.Text != "hello there, this is the whole thought chain." {
		t.Fatalf("event = %+v, want a dictation event with the id and the text", ev)
	}
}

func TestSecondStartStopsTheFirst(t *testing.T) {
	mic := &fakeMic{chunks: [][]byte{pcm(160)}}
	tx := &fakeTranscriber{text: "second"}
	_, srv, _ := newDictationTest(t, mic, tx)

	first := startDictation(t, srv)
	second := startDictation(t, srv)
	if first == second {
		t.Fatal("the second start reused the first id")
	}

	resp := stopDictation(t, srv, first)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("stopping the abandoned dictation = %d, want 404", resp.StatusCode)
	}
	if n := tx.callCount(); n != 0 {
		t.Fatalf("the abandoned dictation was transcribed %d times, want 0", n)
	}

	resp2 := stopDictation(t, srv, second)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("stopping the live dictation = %d, want 200", resp2.StatusCode)
	}
	if mic.opened != 2 {
		t.Fatalf("the microphone was opened %d times, want 2", mic.opened)
	}
}

func TestDictationCapTranscribesWhatThereIs(t *testing.T) {
	mic := &fakeMic{chunks: [][]byte{pcm(3000), pcm(3000), pcm(3000)}}
	tx := &fakeTranscriber{text: "capped"}
	d, srv, events := newDictationTest(t, mic, tx)
	// Two hundred milliseconds at 16 kHz is 3200 samples, so the second chunk trips the cap.
	d.max = 200 * time.Millisecond

	id := startDictation(t, srv)
	ev := mustEvent(t, events)
	if ev.ID != id || ev.Type != "dictation" || ev.Text != "capped" {
		t.Fatalf("event = %+v, want the capped dictation's text", ev)
	}
	// The cap keeps whole chunks, so it stops at the first one that reaches the limit rather than cutting mid-chunk.
	tx.mu.Lock()
	got := len(tx.wav) - 44
	tx.mu.Unlock()
	if want := 6000 * 2; got != want {
		t.Fatalf("capped WAV holds %d bytes of samples, want %d", got, want)
	}
	if !mic.wasClosed() {
		t.Error("the microphone was left open after the cap")
	}

	resp := stopDictation(t, srv, id)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("stop after the cap = %d, want 404", resp.StatusCode)
	}
}

func TestDictationWAVHeader(t *testing.T) {
	mic := &fakeMic{chunks: [][]byte{pcm(1000), pcm(600)}}
	tx := &fakeTranscriber{text: "ok"}
	_, srv, _ := newDictationTest(t, mic, tx)

	id := startDictation(t, srv)
	stopDictation(t, srv, id).Body.Close()

	w := tx.wav
	if len(w) != 44+1600*2 {
		t.Fatalf("WAV is %d bytes, want %d", len(w), 44+1600*2)
	}
	if !bytes.HasPrefix(w, []byte("RIFF")) || string(w[8:16]) != "WAVEfmt " || string(w[36:40]) != "data" {
		t.Fatalf("WAV header chunks are wrong: %q", w[:44])
	}
	if got := binary.LittleEndian.Uint16(w[22:]); got != 1 {
		t.Errorf("channels = %d, want 1", got)
	}
	if got := binary.LittleEndian.Uint32(w[24:]); got != dictateRate {
		t.Errorf("sample rate = %d, want %d", got, dictateRate)
	}
	if got := binary.LittleEndian.Uint16(w[34:]); got != 16 {
		t.Errorf("bits per sample = %d, want 16", got)
	}
	if got := binary.LittleEndian.Uint32(w[40:]); got != uint32(1600*2) {
		t.Errorf("data size = %d, want %d", got, 1600*2)
	}
	if got := binary.LittleEndian.Uint32(w[4:]); got != uint32(36+1600*2) {
		t.Errorf("RIFF size = %d, want %d", got, 36+1600*2)
	}
	if !bytes.Equal(w[44:], append(pcm(1000), pcm(600)...)) {
		t.Error("the samples in the WAV are not the ones the microphone delivered")
	}
}

func TestResampleTo16k(t *testing.T) {
	// Twenty-four thousand samples of anything is one second, and one second at 16 kHz is sixteen thousand samples.
	if got := len(resampleTo16k(pcm(24000), 24000)) / 2; got != 16000 {
		t.Errorf("24 kHz second resampled to %d samples, want 16000", got)
	}
	if got := len(resampleTo16k(pcm(48000), 48000)) / 2; got != 16000 {
		t.Errorf("48 kHz second resampled to %d samples, want 16000", got)
	}
	in := pcm(1000)
	if got := resampleTo16k(in, 16000); !bytes.Equal(got, in) {
		t.Error("audio already at 16 kHz was changed")
	}
	// A ramp stays a ramp: the interpolation must not scramble the order of the samples.
	ramp := make([]byte, 24*2)
	for i := 0; i < 24; i++ {
		binary.LittleEndian.PutUint16(ramp[i*2:], uint16(int16(i*100)))
	}
	out := resampleTo16k(ramp, 24000)
	if len(out)/2 != 16 {
		t.Fatalf("ramp resampled to %d samples, want 16", len(out)/2)
	}
	for i := 1; i < len(out)/2; i++ {
		if int16(binary.LittleEndian.Uint16(out[i*2:])) <= int16(binary.LittleEndian.Uint16(out[(i-1)*2:])) {
			t.Fatalf("the resampled ramp is not increasing at sample %d", i)
		}
	}
}

// tonePCM builds n samples of a square wave at the given amplitude, so a test can hand the gate audio that is plainly speech or plainly a quiet room.
func tonePCM(n int, amplitude int16) []byte {
	b := make([]byte, n*2)
	for i := 0; i < n; i++ {
		v := amplitude
		if i%16 < 8 {
			v = -amplitude
		}
		binary.LittleEndian.PutUint16(b[i*2:], uint16(v))
	}
	return b
}

func TestDictationStopsItselfWhenTheTalkingStops(t *testing.T) {
	// Six hundred milliseconds of speech at 16 kHz is 9600 samples, and the 1.2 seconds of quiet that ends it is 19200.
	mic := &fakeMic{chunks: [][]byte{
		tonePCM(9600, 6000),
		tonePCM(9600, 20),
		tonePCM(9600, 20),
		tonePCM(9600, 20),
	}}
	tx := &fakeTranscriber{text: "book the venue for Tuesday"}
	_, srv, events := newDictationTest(t, mic, tx)

	id := startDictation(t, srv)
	ev := mustEvent(t, events)
	if ev.ID != id || ev.Type != "dictation" || ev.Text != "book the venue for Tuesday" {
		t.Fatalf("event = %+v, want the self-stopped dictation's text", ev)
	}
	if !mic.wasClosed() {
		t.Error("the microphone was left open after the silence stopped the dictation")
	}
	// The window has nothing left to stop: the text already went out on the event stream.
	resp := stopDictation(t, srv, id)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("stop after the silence gate = %d, want 404", resp.StatusCode)
	}
}

func TestDictationIntoAQuietRoomKeepsListening(t *testing.T) {
	// Three seconds of quiet, far past the 1.2 second hangover, but nobody ever spoke, so there is nothing to end.
	mic := &fakeMic{chunks: [][]byte{tonePCM(16000, 20), tonePCM(16000, 20), tonePCM(16000, 20)}}
	tx := &fakeTranscriber{text: "nothing"}
	_, srv, events := newDictationTest(t, mic, tx)

	id := startDictation(t, srv)
	select {
	case ev := <-events:
		t.Fatalf("silence alone ended the dictation: %+v", ev)
	case <-time.After(200 * time.Millisecond):
	}
	resp := stopDictation(t, srv, id)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stop after silence alone = %d, want 200", resp.StatusCode)
	}
}

// pausingDictateMic delivers chunks the same way fakeMic does but blocks the first call to Close until the test closes release, so a test can pause the capture goroutine at exactly the point, right after it decides to self-stop, where a real stop request racing in would land. Every later call to Close returns immediately, the same way it would for a stop request that arrives once that first call has already been made.
type pausingDictateMic struct {
	chunks [][]byte

	mu      sync.Mutex
	closes  int
	blocked chan struct{} // closed once the first Close call is the one blocking
	release chan struct{} // the test closes this to let that first Close call return
}

func newPausingDictateMic(chunks [][]byte) *pausingDictateMic {
	return &pausingDictateMic{chunks: chunks, blocked: make(chan struct{}), release: make(chan struct{})}
}

func (m *pausingDictateMic) StartCapture(ctx context.Context) (<-chan []byte, error) {
	ch := make(chan []byte, len(m.chunks)+1)
	for _, c := range m.chunks {
		ch <- c
	}
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

func (m *pausingDictateMic) Close() error {
	m.mu.Lock()
	first := m.closes == 0
	m.closes++
	m.mu.Unlock()
	if first {
		close(m.blocked)
		<-m.release
	}
	return nil
}

// TestDictationSelfStopOnlyFinishesWhenStillActive covers the race the review found: the silence gate decides to end the recording and cancels the microphone, but a stop request for the same id can land in the moment before the gate clears itself from d.active. That stop finds itself still the active recording, claims it and transcribes. The gate's own path must then see it is no longer the active recording and do nothing more — otherwise the same audio is transcribed and broadcast a second time.
func TestDictationSelfStopOnlyFinishesWhenStillActive(t *testing.T) {
	// The same shape of chunks as TestDictationStopsItselfWhenTheTalkingStops: six hundred milliseconds of speech, then enough quiet to trip the gate.
	mic := newPausingDictateMic([][]byte{
		tonePCM(9600, 6000),
		tonePCM(9600, 20),
		tonePCM(9600, 20),
		tonePCM(9600, 20),
	})
	tx := &fakeTranscriber{text: "book the venue for Tuesday"}
	_, srv, events := newDictationTest(t, mic, tx)

	id := startDictation(t, srv)

	// The gate has tripped and the self-stop path is now blocked in its first mic.Close call — the exact window a racing stop lands in.
	select {
	case <-mic.blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("the silence gate never tripped")
	}

	// The racing stop still finds itself the active recording, so it claims d.active, halts and transcribes.
	resp := stopDictation(t, srv, id)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stop status = %d, want 200", resp.StatusCode)
	}
	ev := mustEvent(t, events)
	if ev.ID != id || ev.Type != "dictation" || ev.Text != "book the venue for Tuesday" {
		t.Fatalf("event = %+v, want the stop's dictation event", ev)
	}

	// Let the self-stop path carry on now that the stop has already finished the recording. It must not transcribe or broadcast a second time.
	close(mic.release)
	select {
	case ev := <-events:
		t.Fatalf("the silence gate finished the recording a second time: %+v", ev)
	case <-time.After(300 * time.Millisecond):
	}
	if n := tx.callCount(); n != 1 {
		t.Fatalf("dictation was transcribed %d times, want 1", n)
	}
}

func TestDictationKeepsListeningThroughAPauseMidSentence(t *testing.T) {
	// Half a second of quiet in the middle of a sentence is a breath, not the end of one: the hangover resets when the talking comes back.
	mic := &fakeMic{chunks: [][]byte{
		tonePCM(9600, 6000),
		tonePCM(8000, 20),
		tonePCM(9600, 6000),
	}}
	tx := &fakeTranscriber{text: "still going"}
	_, srv, events := newDictationTest(t, mic, tx)

	startDictation(t, srv)
	select {
	case ev := <-events:
		t.Fatalf("a mid-sentence pause ended the dictation: %+v", ev)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestDictationTranscribesUnderTheGPULock pins that a dictation's whisper run holds the same lock a meeting transcription holds for the GPU. whisper-medium wants about 2.2 GB on a 4 GB card, so a dictation that starts while a meeting is being transcribed has to queue rather than allocate beside it: on 2026-09-05 three of the four dictations died with "ggml_vulkan: Device memory allocation of size 462323712 failed ... ErrorOutOfDeviceMemory", each one seconds after a meeting-retry ran whisper.
func TestDictationTranscribesUnderTheGPULock(t *testing.T) {
	mic := &fakeMic{chunks: [][]byte{pcm(1600), pcm(1600)}}
	tx := &fakeTranscriber{text: "hello"}
	d, srv, _ := newDictationTest(t, mic, tx)

	var held atomic.Bool
	d.transcribe = func(ctx context.Context, wavPath, prompt string) (string, error) {
		// The lock is held by this dictation exactly when a second attempt to take it cannot succeed.
		if recorder.GPURun.TryLock() {
			recorder.GPURun.Unlock()
		} else {
			held.Store(true)
		}
		return tx.run(ctx, wavPath, prompt)
	}

	id := startDictation(t, srv)
	resp := stopDictation(t, srv, id)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stop status = %d, want 200", resp.StatusCode)
	}
	if !held.Load() {
		t.Error("the whisper run was not made under recorder.GPURun, so a dictation can race a meeting transcription for the card")
	}
	if recorder.GPURun.TryLock() {
		recorder.GPURun.Unlock()
	} else {
		t.Error("the GPU lock was still held after the dictation finished")
	}
}

// A dictation stopped while a meeting holds the GPU waits its turn, and its own three-minute clock starts only once the turn comes, so the wait never eats the decode's budget and the recorded speech is deferred rather than lost.
func TestDictationStopWaitsForTheGPUWithoutSpendingItsClock(t *testing.T) {
	mic := &fakeMic{chunks: [][]byte{pcm(1600)}}
	tx := &fakeTranscriber{text: "later"}
	_, srv, _ := newDictationTest(t, mic, tx)
	id := startDictation(t, srv)

	recorder.GPURun.Lock()
	done := make(chan *http.Response, 1)
	go func() { done <- stopDictation(t, srv, id) }()
	time.Sleep(300 * time.Millisecond)
	released := time.Now()
	recorder.GPURun.Unlock()

	resp := <-done
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stop status = %d, want 200 once the GPU was free", resp.StatusCode)
	}
	tx.mu.Lock()
	deadline := tx.deadline
	tx.mu.Unlock()
	if deadline.Before(released.Add(dictateTimeout - 100*time.Millisecond)) {
		t.Errorf("decode deadline %v was set before the GPU was released at %v; the wait spent the decode's own budget", deadline, released)
	}
}

// fakeDictateWhisper writes a stand-in for whisper-cli at $ORA_WHISPER_CPP and returns the path of the log each run appends to. The first run dies the way the real one died on 2026-09-06 — the Vulkan allocation failure on stderr and then SIGSEGV — and every run after it prints stdout and exits 0, unless alwaysFail is set, in which case every run dies. Each run logs whether the WAV it was handed is still on disk, so a file deleted between the attempts shows up.
func fakeDictateWhisper(t *testing.T, stdout string, alwaysFail bool) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-whisper")
	log := filepath.Join(dir, "runs.log")
	fail := "if [ ! -f " + dir + "/ran ]; then\n"
	if alwaysFail {
		fail = "if true; then\n"
	}
	body := "#!/bin/sh\n" +
		"if [ -f \"$2\" ]; then echo \"wav-present $@\" >> " + log + "; else echo \"wav-gone $@\" >> " + log + "; fi\n" +
		fail +
		"  touch " + dir + "/ran\n" +
		"  echo 'ggml_vulkan: Device memory allocation of size 462323712 failed.' >&2\n" +
		"  echo 'ggml_vulkan: vk::Device::allocateMemory: ErrorOutOfDeviceMemory' >&2\n" +
		"  kill -SEGV $$\n" +
		"fi\n" +
		"cat <<'EOF'\n" + stdout + "\nEOF\nexit 0\n"
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake whisper: %v", err)
	}
	t.Setenv("ORA_WHISPER_CPP", bin)
	return log
}

// dictateRuns returns one line per run the fake whisper logged, in order.
func dictateRuns(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("read the fake whisper's run log: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// A dictation whose GPU run dies for want of card memory is decoded again on the CPU, and what comes back to the window is the words rather than the error the first attempt hit. The audio is what the user said and cannot be recorded again, so it has to survive until both attempts are done.
func TestDictationReportsTheWordsFromTheCPURetry(t *testing.T) {
	log := fakeDictateWhisper(t, "shall we ship on friday", false)
	mic := &fakeMic{chunks: [][]byte{pcm(1600), pcm(1600)}}
	d, srv, _ := newDictationTest(t, mic, &fakeTranscriber{})
	d.transcribe = whisperText

	id := startDictation(t, srv)
	resp := stopDictation(t, srv, id)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stop status = %d, want 200: the CPU retry transcribed the audio", resp.StatusCode)
	}
	var body struct{ Text string }
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode stop body: %v", err)
	}
	if body.Text != "shall we ship on friday" {
		t.Fatalf("text = %q, want the words the CPU retry transcribed", body.Text)
	}

	runs := dictateRuns(t, log)
	if len(runs) != 2 {
		t.Fatalf("whisper ran %d times, want 2: the GPU run that died and the CPU retry\n%v", len(runs), runs)
	}
	for i, r := range runs {
		if !strings.HasPrefix(r, "wav-present") {
			t.Errorf("run %d found no audio to transcribe: %s", i, r)
		}
	}
	if strings.Contains(runs[0], " -ng") {
		t.Errorf("the first run was already on the CPU: %s", runs[0])
	}
	if !strings.Contains(runs[1], " -ng") {
		t.Errorf("the retry did not turn the GPU off, so it fails for the same reason: %s", runs[1])
	}
}

// When the CPU retry fails too there is nothing to report but the failure, and the user has to see it rather than an empty dictation.
func TestWhisperTextReportsTheErrorWhenBothAttemptsFail(t *testing.T) {
	log := fakeDictateWhisper(t, "", true)
	wav := filepath.Join(t.TempDir(), "dictation.wav")
	if err := writeWAV(wav, pcm(1600)); err != nil {
		t.Fatalf("writeWAV: %v", err)
	}

	text, err := whisperText(context.Background(), wav, "")
	if err == nil {
		t.Fatalf("whisperText returned %q and no error, want the failure of both attempts", text)
	}
	if !strings.Contains(err.Error(), "ErrorOutOfDeviceMemory") {
		t.Errorf("error = %v, want whisper's own account of why it failed", err)
	}
	if runs := dictateRuns(t, log); len(runs) != 2 {
		t.Fatalf("whisper ran %d times, want 2: the GPU run and the CPU retry\n%v", len(runs), runs)
	}
}
