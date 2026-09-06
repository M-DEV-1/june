package ipc

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ora/internal/audio"
	"ora/internal/config"
	"ora/internal/recorder"
	"ora/internal/util"
)

// micRate is the sample rate internal/audio's microphone delivers: its PulseAudio record stream is opened at 24 kHz mono s16le (see StartCapture in internal/audio/capture_linux.go), so everything captured here has to come down to whisper's rate before it is written.
const micRate = 24000

// dictateRate is the sample rate whisper wants, and the rate every WAV this file writes is stamped with.
const dictateRate = 16000

// The silence gate: dictation is a toggle now, so the daemon ends a recording itself once the speaking is plainly over rather than waiting for the second key press. A chunk counts as speech when its RMS reaches dictateSpeechRMS, and the recording ends once at least dictateMinSpeech of speech has been heard and dictateSilenceStop has passed since the last of it. Variables rather than constants because the threshold is a calibration against real hardware — a hot laptop microphone in a noisy room reads far above a quiet headset — and because the tests shrink them.
var (
	// Full scale for 16-bit audio is 32768. A quiet room reads a few tens, ordinary speech a couple of thousand, so 500 sits between them with room on both sides.
	dictateSpeechRMS = 500.0
	// Enough speech to be sure somebody said something rather than knocked the desk.
	dictateMinSpeech = 600 * time.Millisecond
	// A breath mid-sentence is shorter than this; the end of a thought is longer.
	dictateSilenceStop = 1200 * time.Millisecond
)

// maxDictation is how long one dictation may run before it is transcribed whether or not anyone ever stopped it — a window that goes away mid-dictation, or a room too noisy for the silence gate to trip, otherwise holds the microphone for ever.
const maxDictation = 120 * time.Second

// dictateTimeout bounds one whisper run so a hung decode ends with an error rather than holding the route's goroutine.
const dictateTimeout = 3 * time.Minute

// promptTimeout bounds the store reads that build the priming prompt, which happen before the GPU turn is taken.
const promptTimeout = 10 * time.Second

// micSource is the part of audio.Microphone dictation uses, narrowed so the tests can feed canned PCM instead of opening the machine's microphone.
type micSource interface {
	StartCapture(ctx context.Context) (<-chan []byte, error)
	Close() error
}

// Dictation serves /dictate/start and /dictate/stop: press a key, speak, and get the words back, either from the stop this window sends or from the silence gate ending the recording on its own. One recording is open at a time; starting a second ends the first.
type Dictation struct {
	hub *hub

	// Seams, all set by NewDictation and replaced in tests: opening the microphone, running whisper over a WAV, building the priming prompt, and the rate the microphone delivers.
	openMic    func() (micSource, error)
	transcribe func(ctx context.Context, wavPath, prompt string) (string, error)
	prompt     func(ctx context.Context) string
	rate       int
	max        time.Duration

	mu     sync.Mutex
	active *dictating
	nextID atomic.Uint64
}

// dictating is one open recording: the microphone it holds, the audio captured so far, and the cancel that stops the capture.
type dictating struct {
	id     string
	cancel context.CancelFunc
	mic    micSource
	done   chan struct{} // closed once the capture goroutine has drained the microphone

	mu  sync.Mutex
	pcm []byte // raw s16le at the microphone's own rate, resampled only when the recording ends
}

// NewDictation builds the dictation routes on top of an existing Server, borrowing its hub so a finished dictation is broadcast on the same /events stream the window already listens to, and its store for the names whisper is primed with. Input: the server. Output: the handler pair; register Start and Stop on a mux (see cmd/daemon.go for the route names).
func NewDictation(s *Server) *Dictation {
	d := &Dictation{
		hub:        s.hub,
		openMic:    func() (micSource, error) { return audio.NewMic() },
		transcribe: whisperText,
		rate:       micRate,
		max:        maxDictation,
	}
	d.prompt = func(ctx context.Context) string {
		if s.store == nil {
			return ""
		}
		entries, err := s.store.PersonalContext(ctx)
		if err != nil {
			slog.Warn("could not read personal context to prime dictation", "error", err)
			return ""
		}
		subjects := make([]string, 0, len(entries))
		for _, e := range entries {
			subjects = append(subjects, e.Subject)
		}
		return dictationPromptFrom(subjects)
	}
	return d
}

// Start handles POST /dictate/start: it opens the microphone and begins buffering what is said. Input: no body. Output: 202 with JSON {"id": string}, the id every later stop and every event for this recording carries. A recording already open is ended first and its audio thrown away, so a second start always wins.
func (d *Dictation) Start(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	prev := d.active
	d.active = nil
	d.mu.Unlock()
	if prev != nil {
		prev.halt()
	}

	mic, err := d.openMic()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	chunks, err := mic.StartCapture(ctx)
	if err != nil {
		cancel()
		mic.Close()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	cur := &dictating{
		id:     fmt.Sprintf("dictate-%d", d.nextID.Add(1)),
		cancel: cancel,
		mic:    mic,
		done:   make(chan struct{}),
	}
	d.mu.Lock()
	d.active = cur
	d.mu.Unlock()

	go d.capture(cur, chunks)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"id": cur.id})
}

// chunkRMS is the root mean square of one block of mono s16le samples, which is what the silence gate compares against dictateSpeechRMS. Input: the raw little-endian bytes. Output: the RMS in the same units as the samples themselves, 0 for an empty chunk.
func chunkRMS(pcm []byte) float64 {
	n := len(pcm) / 2
	if n == 0 {
		return 0
	}
	var sum float64
	for i := 0; i < n; i++ {
		v := float64(int16(binary.LittleEndian.Uint16(pcm[i*2:])))
		sum += v * v
	}
	return math.Sqrt(sum / float64(n))
}

// capture drains the microphone into the recording's buffer and ends it either at the time cap or once the silence gate says the speaking is over, transcribing what there is rather than throwing it away. Input: the open recording and its channel of PCM chunks. Output: nothing; done is closed when the microphone has been drained.
func (d *Dictation) capture(cur *dictating, chunks <-chan []byte) {
	capBytes := int(d.max.Seconds() * float64(d.rate) * 2)
	// How much speech has been heard in total, and how long it has been quiet since the last of it. Both are measured from the audio itself rather than from the clock, so the gate behaves the same against a test's canned chunks as against a live microphone.
	var spoken, quiet time.Duration
	selfStopped := false
	for chunk := range chunks {
		cur.mu.Lock()
		cur.pcm = append(cur.pcm, chunk...)
		full := len(cur.pcm) >= capBytes
		cur.mu.Unlock()

		span := time.Duration(len(chunk)/2) * time.Second / time.Duration(d.rate)
		if chunkRMS(chunk) >= dictateSpeechRMS {
			spoken += span
			quiet = 0
		} else {
			quiet += span
		}
		if full || (spoken >= dictateMinSpeech && quiet >= dictateSilenceStop) {
			// Past the cap, or past the end of what was being said, there is nothing left to gain from the rest of the stream, so the microphone is stopped and the loop left rather than drained. On a stop from the window it is the other way round: the loop keeps going until the channel closes, because everything still buffered there is audio the user spoke.
			selfStopped = true
			cur.cancel()
			break
		}
	}
	close(cur.done)
	if !selfStopped {
		return
	}
	// Nobody is coming with a stop for this one, so it closes the microphone itself. Whether it also finishes the recording depends on the check below: a stop for this same id can land between the gate deciding to end it here and that check running, in which case Stop() has already halted and transcribed it. Finishing regardless of that check would transcribe and broadcast the same audio twice.
	cur.mic.Close()
	d.mu.Lock()
	stillActive := d.active == cur
	if stillActive {
		d.active = nil
	}
	d.mu.Unlock()
	if !stillActive {
		return
	}
	if _, err := d.finish(cur); err != nil {
		slog.Error("dictation ended on its own and could not be transcribed", "id", cur.id, "error", err)
	}
}

// Stop handles POST /dictate/stop: it closes the microphone, transcribes what was said and returns it. Input: JSON body {"id": string}, the id start returned. Output: 200 with JSON {"text": string}, the transcript trimmed; 404 when that dictation is not the one open (nothing was started, it was superseded by a later start, or it already ended on its own time limit — in which case its text went out on /events instead); 500 when whisper failed.
func (d *Dictation) Stop(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	d.mu.Lock()
	cur := d.active
	if cur == nil || (req.ID != "" && req.ID != cur.id) {
		d.mu.Unlock()
		http.Error(w, "no dictation is open", http.StatusNotFound)
		return
	}
	d.active = nil
	d.mu.Unlock()

	cur.halt()
	text, err := d.finish(cur)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"text": text})
}

// halt stops the microphone and waits for every chunk it already handed over to be buffered.
func (c *dictating) halt() {
	c.cancel()
	<-c.done
	c.mic.Close()
}

// finish turns a finished recording into text: the buffered audio is resampled to 16 kHz, written to a temp WAV, run through whisper with the priming prompt, and the file deleted. The result is broadcast on the hub as a "dictation" event so a window that lost the HTTP response still gets its words. Input: the recording, already halted. Output: the transcript, trimmed.
func (d *Dictation) finish(cur *dictating) (string, error) {
	cur.mu.Lock()
	raw := cur.pcm
	cur.mu.Unlock()

	f, err := os.CreateTemp("", "ora-dictation-*.wav")
	if err != nil {
		return "", fmt.Errorf("create dictation wav: %w", err)
	}
	path := f.Name()
	f.Close()
	defer os.Remove(path)
	if err := writeWAV(path, resampleTo16k(raw, d.rate)); err != nil {
		return "", err
	}

	started := time.Now()
	promptCtx, cancelPrompt := context.WithTimeout(context.Background(), promptTimeout)
	prompt := d.prompt(promptCtx)
	cancelPrompt()
	// whisper decodes on the GPU and only one run fits on this card, so a dictation started while a meeting is being transcribed waits its turn rather than failing to allocate beside it. Three of the four dictations on 2026-09-05 died with ErrorOutOfDeviceMemory for want of this. The decode's own clock starts only once the turn comes, so a long wait behind a meeting defers the dictation instead of spending its whole budget in the queue and then failing at once.
	recorder.GPURun.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), dictateTimeout)
	text, err := d.transcribe(ctx, path, prompt)
	cancel()
	recorder.GPURun.Unlock()
	if err != nil {
		return "", err
	}
	text = strings.TrimSpace(text)
	slog.Info("dictation transcribed", "id", cur.id, "audio", time.Duration(len(raw)/2)*time.Second/time.Duration(d.rate), "took", time.Since(started), "chars", len(text))

	d.hub.broadcast(Event{ID: cur.id, Type: "dictation", Text: text, Evidence: []EvidenceItem{}, Actions: []ActionItem{}})
	return text, nil
}

// resampleTo16k converts mono s16le PCM down to 16 kHz by interpolating linearly between neighbouring samples. Input: the raw little-endian 16-bit samples and the rate they were captured at. Output: the same audio at 16 kHz, or the input untouched when it is already there.
// ponytail: no anti-alias filter, so anything above 8 kHz in the source folds back into the result. Speech carries almost nothing up there and whisper's own resampler is no better, but if a dictation from a bright microphone ever comes back hissy, a low-pass before the decimation is the fix.
func resampleTo16k(pcm []byte, rate int) []byte {
	if rate == dictateRate || rate <= 0 {
		return pcm
	}
	in := len(pcm) / 2
	sample := func(i int) float64 { return float64(int16(binary.LittleEndian.Uint16(pcm[i*2:]))) }
	out := in * dictateRate / rate
	buf := make([]byte, out*2)
	for j := 0; j < out; j++ {
		pos := float64(j) * float64(rate) / float64(dictateRate)
		i := int(pos)
		v := sample(i)
		if i+1 < in {
			v += (pos - float64(i)) * (sample(i+1) - v)
		}
		binary.LittleEndian.PutUint16(buf[j*2:], uint16(int16(v)))
	}
	return buf
}

// wavHeaderSize is the canonical RIFF/fmt/data header: 12 bytes RIFF + 24 bytes fmt + 8 bytes data. Nothing else goes in it — whisper's WAV reader rejects a file with any other chunk between fmt and data.
const wavHeaderSize = 44

// whisperFrameChunk is the block size whisper's decoder reads in. A file whose frame count is an exact multiple of it fails to decode — the final read comes back "At end" and whisper reports it could not read the file — so one frame is dropped when that happens, at a cost of 1/16000 of a second.
const whisperFrameChunk = 512

// writeWAV writes 16 kHz mono s16le samples to path as a WAV with the canonical 44-byte header. Input: the path and the samples. Output: an error if the file could not be written.
func writeWAV(path string, pcm []byte) error {
	n := len(pcm)
	if frames := n / 2; frames > 0 && frames%whisperFrameChunk == 0 {
		n -= 2
	}
	h := make([]byte, wavHeaderSize)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+n))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)          // fmt chunk size
	binary.LittleEndian.PutUint16(h[20:], 1)           // PCM
	binary.LittleEndian.PutUint16(h[22:], 1)           // mono
	binary.LittleEndian.PutUint32(h[24:], dictateRate) // sample rate
	binary.LittleEndian.PutUint32(h[28:], dictateRate*2)
	binary.LittleEndian.PutUint16(h[32:], 2)  // block align: 1 channel * 2 bytes
	binary.LittleEndian.PutUint16(h[34:], 16) // bits per sample
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(n))
	return os.WriteFile(path, append(h, pcm[:n]...), 0o600)
}

// The decoder thresholds and the model, the same ones internal/recorder passes for a meeting: whisper decodes each window at a rising temperature and keeps the first result that passes these two tests, so tightening them makes it retry a bad window instead of printing what it invented.
const (
	dictateEntropyThreshold = "2.20"
	dictateLogProbThreshold = "-0.70"
	whisperCPPBinaryName    = "whisper-cli"
	whisperCPPModelName     = "ggml-medium.bin"
)

// whisperText runs the machine's whisper.cpp build over one WAV and returns what was said as a single line. Input: the WAV's path and the priming prompt, which biases whisper's spelling towards the words in it (pass "" for none). Output: the text with whisper's non-speech markers dropped and its lines joined by spaces.
// The caller holds recorder.GPURun for the whole run (see finish), so this never allocates on the card beside a meeting's decode.
// ponytail: it still does not wait for the embedding server to yield the way a meeting transcription does, because a dictation is something the user is waiting on and recorder's releaser polls for half a minute at a time. If a dictation ever fails to allocate with nothing but embeds on the card, that releaser is the next thing to export.
func whisperText(ctx context.Context, wavPath, prompt string) (string, error) {
	bin := filepath.Join(config.DataDir(), "whispercpp", whisperCPPBinaryName)
	if p := os.Getenv("ORA_WHISPER_CPP"); p != "" {
		bin = p
	}
	if info, err := os.Stat(bin); err != nil || info.IsDir() {
		return "", fmt.Errorf("no whisper.cpp build at %s: put %s and its model there", bin, whisperCPPBinaryName)
	}

	// -nt drops the timestamps, which a dictation has no use for, so stdout is the words and nothing else.
	args := []string{"-f", wavPath, "-np", "-nt", "-et", dictateEntropyThreshold, "-lpt", dictateLogProbThreshold}
	// No model beside the binary means a stub, which is how a test's bare script gets run without flags it would not understand.
	if model := filepath.Join(filepath.Dir(bin), whisperCPPModelName); fileExists(model) {
		args = append(args, "-m", model)
		if dev := config.LoadConfig().Transcribe.GPUDevice; dev > 0 {
			args = append(args, "-dev", strconv.Itoa(dev))
		}
	}
	if prompt != "" {
		args = append(args, "--prompt", prompt)
	}

	// recorder.RunWhisper redoes the run on the CPU when the card has no memory for it, so a dictation that would have been lost to ErrorOutOfDeviceMemory comes back as words a little later instead. Both attempts happen inside the GPU turn finish is holding, and finish deletes the WAV only once this has returned.
	out, errOut, err := recorder.RunWhisper(ctx, bin, args)
	if err != nil {
		return "", fmt.Errorf("whisper: %w (%s)", err, strings.TrimSpace(errOut))
	}

	var words []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || util.NonSpeechLine.MatchString(line) {
			continue
		}
		words = append(words, line)
	}
	return strings.Join(words, " "), nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// dictationPromptBudget caps the priming prompt in characters, for the same reason internal/recorder caps its own: whisper keeps only the last few hundred tokens of what it is primed with, so a longer prompt has its front silently cut off.
const dictationPromptBudget = 500

// dictationPromptFrom turns the store's personal-context subjects into the sentence whisper is primed with, so a name spoken into the microphone is spelled the way memory already spells it. Input: the subjects, as stored ("sneha-kumar"). Output: a line like "Notes. People: Sneha Kumar, Rohit.", or "" when there is nobody to name; the user's own identity entry and their preferences are skipped, and the sentence is capped at dictationPromptBudget characters.
// It is written as capitalised, punctuated English on purpose: whisper continues the prompt's register as well as its vocabulary, and a bare lowercase word list comes back as a lowercase unpunctuated transcript.
func dictationPromptFrom(subjects []string) string {
	var b strings.Builder
	b.WriteString("Notes. People:")
	named := 0
	for _, s := range subjects {
		if s == "identity" || strings.HasPrefix(s, "preferences") {
			continue
		}
		parts := strings.Split(s, "-")
		for i, w := range parts {
			if w != "" {
				parts[i] = strings.ToUpper(w[:1]) + w[1:]
			}
		}
		name := strings.Join(parts, " ")
		// Two characters of slack for the separator and the closing full stop.
		if b.Len()+len(name)+2 > dictationPromptBudget {
			break
		}
		if named > 0 {
			b.WriteString(",")
		}
		b.WriteString(" " + name)
		named++
	}
	if named == 0 {
		return ""
	}
	b.WriteString(".")
	return b.String()
}
