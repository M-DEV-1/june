package recorder

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"ora/internal/config"
)

// The parakeet engine is NVIDIA's Parakeet TDT 0.6B run through sherpa-onnx's prebuilt offline CLI, as an alternative to whisper. It is installed by hand into <data dir>/parakeet: the sherpa-onnx runner binary, the three ONNX model files and their token list, and the Silero voice-activity model the runner uses to cut the audio into segments.
// Everything lives flat in that one directory, so the whole engine is found from the binary's own path and nothing has to be configured but the choice to use it.
const (
	parakeetBinaryName = "sherpa-onnx-vad-with-offline-asr"
	parakeetVADModel   = "silero_vad.onnx"
)

// parakeetModelFiles are the model files that must sit beside the runner for it to work. They are checked for by name so that a half-finished install fails here, with a message naming the directory, instead of deep inside sherpa-onnx.
var parakeetModelFiles = []string{"encoder.int8.onnx", "decoder.int8.onnx", "joiner.int8.onnx", "tokens.txt", parakeetVADModel}

// parakeetBinary returns the path to the sherpa-onnx runner: $ORA_PARAKEET if set, otherwise <dataDir>/parakeet/sherpa-onnx-vad-with-offline-asr, but only when every model file is present beside it.
func parakeetBinary(dataDir string) (string, error) {
	dir := filepath.Join(dataDir, "parakeet")
	bin := filepath.Join(dir, parakeetBinaryName)
	if p := os.Getenv("ORA_PARAKEET"); p != "" {
		bin = p
		dir = filepath.Dir(p)
	}
	if info, err := os.Stat(bin); err != nil || info.IsDir() {
		return "", fmt.Errorf("no parakeet runner at %s: install sherpa-onnx's %s there, or set the transcribe engine back to whisper", bin, parakeetBinaryName)
	}
	for _, name := range parakeetModelFiles {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return "", fmt.Errorf("the parakeet runner at %s is missing %s beside it", bin, name)
		}
	}
	return bin, nil
}

// transcribeParakeetWAV runs Parakeet over one WAV and returns its segments, labelled with speaker and shifted by offset — how much later this stream started than the recording as a whole. It matches transcribeWAV's signature so the two engines are interchangeable behind the recorder's seam.
// prompt is accepted and ignored. This is a real loss against whisper: Parakeet is a transducer with no prompt conditioning, so the meeting's own acronyms and participant names cannot be fed to it the way primingPrompt feeds them to whisper, and it will spell an unfamiliar name however it sounds. sherpa-onnx does have a --hotwords-file for contextual biasing, but the published TDT bundle ships no BPE vocabulary, which that path needs.
func transcribeParakeetWAV(ctx context.Context, bin, path, speaker, prompt string, offset time.Duration) ([]Segment, error) {
	if err := repairWAV(path); err != nil {
		return nil, fmt.Errorf("repair %s: %w", path, err)
	}
	dir := filepath.Dir(bin)
	model := func(name string) string { return filepath.Join(dir, name) }
	args := []string{
		"--silero-vad-model=" + model(parakeetVADModel),
		"--tokens=" + model("tokens.txt"),
		"--encoder=" + model("encoder.int8.onnx"),
		"--decoder=" + model("decoder.int8.onnx"),
		"--joiner=" + model("joiner.int8.onnx"),
		"--model-type=nemo_transducer",
		"--num-threads=" + strconv.Itoa(transcribeThreads()),
		path,
	}
	started := time.Now()
	out, errOut, err := run(ctx, bin, args)
	took := time.Since(started)
	slog.Debug("parakeet finished", "file", filepath.Base(path), "took", took, "stderr", strings.TrimSpace(lastLines(errOut, 3)))
	if err != nil {
		return nil, fmt.Errorf("parakeet %s: %w (%s)", filepath.Base(path), err, strings.TrimSpace(lastLines(errOut, 3)))
	}
	// There is deliberately no speed check here, unlike whisper's. The voice-activity pass drops the silence before the model ever sees it, so a quiet meeting genuinely does come back in a fraction of the time its audio would take to play, and whisper's "too fast to be real" rule would call every silent recording a failure.
	return parseParakeetSegments(out, speaker, offset), nil
}

// parakeetSegmentLine matches one line of sherpa-onnx's stdout, e.g. "2.758 -- 4.140: Yes, can you hear me?". The times are seconds from the start of the file. sherpa-onnx keeps its banners on stderr, so stdout is segments and nothing else, but the line is still anchored at both ends so a stray line can never be read as speech.
var parakeetSegmentLine = regexp.MustCompile(`^(\d+(?:\.\d+)?) -- (\d+(?:\.\d+)?): (.*)$`)

// parseParakeetSegments pulls the timestamped segments out of sherpa-onnx's stdout, dropping anything that is not a segment line and any segment with no words in it. Every timestamp is shifted by offset so segments from both streams share one clock.
// The same repeat-collapse whisper gets is applied at the end: a transducer loops far less often than whisper does, not never, and the guard costs nothing on a transcript that never looped.
func parseParakeetSegments(out, speaker string, offset time.Duration) []Segment {
	var segs []Segment
	for _, line := range strings.Split(out, "\n") {
		m := parakeetSegmentLine.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil {
			continue
		}
		text := strings.TrimSpace(m[3])
		if text == "" || nonSpeech.MatchString(text) {
			continue
		}
		segs = append(segs, Segment{
			Start:   secondsToDuration(m[1]) + offset,
			End:     secondsToDuration(m[2]) + offset,
			Speaker: speaker,
			Text:    text,
		})
	}
	return dropHallucinations(segs)
}

// secondsToDuration turns one of sherpa-onnx's decimal-second timestamps into a duration, rounded to the millisecond — which is all the printed value carries, and which keeps 4.140 from landing on 4.139999999. An unparseable value becomes zero rather than dropping the segment, since the words are worth more than the exact time they were said at.
func secondsToDuration(s string) time.Duration {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return time.Duration(math.Round(f*1000)) * time.Millisecond
}

// lastLines returns the final n non-empty lines of s. sherpa-onnx prints its entire configuration on every run, several thousand characters of it, and only the tail says what actually went wrong.
func lastLines(s string, n int) string {
	var kept []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			kept = append(kept, line)
		}
	}
	if len(kept) > n {
		kept = kept[len(kept)-n:]
	}
	return strings.Join(kept, "\n")
}

// The whisper.cpp engine is the same whisper model the whisperfile runs, but as a separately built binary that can decode on the GPU through its Vulkan backend. It is installed into <data dir>/whispercpp: the whisper-cli binary and the ggml model beside it.
// Unlike a whisperfile, which has its weights baked into the executable, whisper-cli has to be told which model to load, so the only difference at the call site is a couple of extra flags.
const (
	whisperCPPBinaryName = "whisper-cli"
	whisperCPPModelName  = "ggml-medium.bin"
)

// whisperCPPBinary returns the path to the whisper.cpp build: $ORA_WHISPER_CPP if set, otherwise <dataDir>/whispercpp/whisper-cli, but only when the model is present beside it.
func whisperCPPBinary(dataDir string) (string, error) {
	dir := filepath.Join(dataDir, "whispercpp")
	bin := filepath.Join(dir, whisperCPPBinaryName)
	if p := os.Getenv("ORA_WHISPER_CPP"); p != "" {
		bin = p
		dir = filepath.Dir(p)
	}
	if info, err := os.Stat(bin); err != nil || info.IsDir() {
		return "", fmt.Errorf("no whisper.cpp build at %s: put one there, or set the transcribe engine back to whisper", bin)
	}
	if _, err := os.Stat(filepath.Join(dir, whisperCPPModelName)); err != nil {
		return "", fmt.Errorf("the whisper.cpp build at %s is missing %s beside it", bin, whisperCPPModelName)
	}
	return bin, nil
}

// whisperCPPArgs returns the extra flags a whisper.cpp build needs and a whisperfile does not: the model to load, and which GPU to load it onto. It returns nil when there is no model beside the binary, which is how a whisperfile is told apart from a whisper.cpp build without asking the config twice.
// The GPU number matters on a laptop with two of them: whisper.cpp counts every Vulkan device it can see and defaults to the first, which on this machine is the integrated graphics — several times slower than the discrete card sitting next to it. The number is per-machine, so it is configured rather than guessed.
func whisperCPPArgs(bin string) []string {
	model := filepath.Join(filepath.Dir(bin), whisperCPPModelName)
	if _, err := os.Stat(model); err != nil {
		return nil
	}
	args := []string{"-m", model}
	if d := config.LoadConfig().Transcribe.GPUDevice; d > 0 {
		args = append(args, "-dev", strconv.Itoa(d))
	}
	return args
}

// gpuRun serializes whisper.cpp runs against each other. whisper-medium takes about 2.2 GB of GPU memory, and this machine's card has 4 GB with a share of it already spoken for, so the two streams of a call cannot be decoded at the same time — the second would fail to allocate. Running them one after the other costs nothing, because on the GPU the card is the bottleneck rather than the number of streams.
// Whisperfiles are not held by this: they decode on the CPU, where running both streams at once is exactly the point.
var gpuRun sync.Mutex
