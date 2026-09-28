package recorder

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"june/internal/config"
	"june/internal/util"
)

// The transcription engine is whisper-medium through a whisper.cpp build, which decodes on the GPU through its Vulkan backend and falls back to the CPU on its own when no GPU is there. It is installed into <data dir>/whispercpp: the whisper-cli binary and the ggml model beside it.
// whisper-cli has to be told which model to load, so the call site adds a couple of flags on top of the shared decoder thresholds.
const (
	whisperCPPBinaryName = "whisper-cli"
	whisperCPPModelName  = "ggml-medium.bin"
	// whisperVADModelName is the Silero voice-activity model whisper.cpp loads with --vad. It is optional: without it the decoder is handed the whole stream, as it was before.
	whisperVADModelName = "ggml-silero-v6.2.0.bin"
)

// WhisperCPPBinary returns the path to the whisper.cpp build: $JUNE_WHISPER_CPP if set, otherwise <dataDir>/whispercpp/whisper-cli, but only when the model is present beside it. The error names the exact path of each missing file, which is also what june doctor prints.
func WhisperCPPBinary(dataDir string) (string, error) {
	bin := whisperCPPPath(dataDir)
	model := filepath.Join(filepath.Dir(bin), whisperCPPModelName)
	if info, err := os.Stat(bin); err != nil || info.IsDir() {
		return "", fmt.Errorf("no whisper-cli at %s, and it needs %s beside it", bin, model)
	}
	if _, err := os.Stat(model); err != nil {
		return "", fmt.Errorf("no %s at %s, beside whisper-cli", whisperCPPModelName, model)
	}
	return bin, nil
}

// WhisperVADModel returns where the Silero voice-activity model is looked for: beside whisper-cli, so in $JUNE_WHISPER_CPP's directory when that is set and in <dataDir>/whispercpp otherwise.
func WhisperVADModel(dataDir string) string {
	return filepath.Join(filepath.Dir(whisperCPPPath(dataDir)), whisperVADModelName)
}

// whisperCPPPath returns where whisper-cli is looked for, whether or not it is there: $JUNE_WHISPER_CPP if set, otherwise <dataDir>/whispercpp/whisper-cli.
func whisperCPPPath(dataDir string) string {
	if p := os.Getenv("JUNE_WHISPER_CPP"); p != "" {
		return p
	}
	return filepath.Join(dataDir, "whispercpp", whisperCPPBinaryName)
}

// whisperCPPArgs returns the flags that point whisper-cli at its model and its GPU, or nil when no model sits beside the binary — which is how a test's bare stub script gets run without flags it would not understand.
// The GPU number matters on a laptop with two of them: whisper.cpp counts every Vulkan device it can see and defaults to the first, which on this machine is the integrated graphics — several times slower than the discrete card sitting next to it. The number is per-machine, so it is configured rather than guessed.
// -dev N is how this build takes it (whisper-cli --help lists it beside -ng), which is why nothing here sets GGML_VK_VISIBLE_DEVICES: that variable hides devices and renumbers the ones left, so it changes the meaning of every number rather than naming one.
// Which number is the discrete card cannot be worked out from here. The order is whatever the Vulkan loader enumerates, and it moves with a driver update, a BIOS graphics setting or an external card being plugged in, so "device 1 is the NVIDIA" is a fact about this machine today and belongs in the config. What the code does instead of trusting the order is survive a bad guess: a decode that dies on the chosen device is redone on the CPU (see RunWhisper).
func whisperCPPArgs(bin string) []string {
	model := filepath.Join(filepath.Dir(bin), whisperCPPModelName)
	if _, err := os.Stat(model); err != nil {
		return nil
	}
	// -l auto, because whisper-cli's own default is -l en and that is not a preference: told the audio is English, it prints [NON-ENGLISH SPEECH] for every stretch that is not, and those lines are dropped as markers. The Hindi call on the evening of 16 September 2026 reached the minutes as an empty transcript that way — 456 of 461 lines in a ten-minute slice were that marker — and the write-up said nobody spoke. Auto-detection transcribes the same audio properly. --translate is not added with it: on that slice it went back to printing the marker.
	args := []string{"-m", model, "-l", "auto"}
	if d := config.LoadConfig().Transcribe.GPUDevice; d > 0 {
		args = append(args, "-dev", strconv.Itoa(d))
	}
	// Voice activity detection, when its model is installed. Whisper decodes in 30-second windows and carries what it decoded into the next one, so a stream that is mostly quiet teaches it that this stream is not speech: on the 16 September 2026 standup it printed "[waves crashing]" for 23 minutes and never transcribed the 51 seconds of the user's own update sitting in the middle, and the minutes said he did not speak. Cutting the stream to the spans Silero calls speech recovered the whole update, and made the run 29 seconds instead of 83.
	// The decoder thresholds and the priming prompt do not help here: the same file transcribes to nothing at 40x gain, with -sns, and unprimed, while the 60 seconds around the speech transcribe perfectly untouched. The silence around the speech is the problem, so the fix is to stop handing it over.
	if vad := filepath.Join(filepath.Dir(bin), whisperVADModelName); util.Exists(vad) {
		args = append(args, "--vad", "-vm", vad)
	}
	return args
}

// RunWhisper runs one whisper-cli command and, when that run dies for want of GPU memory, runs the same command once more with the GPU switched off rather than losing the recording. Input: the context, the whisper-cli binary and the arguments of the GPU run. Output: the stdout, stderr and error of whichever run answered — the CPU one whenever there was a retry.
// Both attempts happen inside the caller's GPU turn: transcribeWAV and internal/ipc's Dictation.finish each hold GPURun across their whole decode, so the retry cannot race the embedding server or the other stream any more than the first attempt could. The audio file is the caller's too, and it deletes it only once this returns.
func RunWhisper(ctx context.Context, bin string, args []string) (stdout, stderr string, err error) {
	// Held across both attempts: the CPU retry is what the first attempt's crash cost us, and letting the embedder back on the card halfway would only set up the next crash.
	gpuHeld.Store(true)
	defer gpuHeld.Store(false)
	// Claim the card first, then ask its other tenant to leave: gpuHeld is what stops the embedding server climbing straight back on, so raising it before the ask is what makes the ask stick.
	releaseTheCard()
	out, errOut, err := run(ctx, bin, args)
	if !gpuOutOfMemory(ctx, err, out+errOut) {
		return out, errOut, err
	}
	slog.Warn("whisper died on the GPU, transcribing the same audio again on the CPU", "device", whisperGPUDevice(), "error", err, "stderr", strings.TrimSpace(errOut))
	// -ng is whisper-cli's own switch for decoding without a GPU, and it goes on the end so it wins whatever the first attempt was given.
	return run(ctx, bin, append(append([]string{}, args...), "-ng"))
}

// releaseTheCard asks the GPU's other tenant to yield before a whisper run, and reports whether it went. It asks once and does not poll, because this runs on the dictation path too and somebody is waiting on that: a tenant that will not move immediately costs the run a CPU retry, which is slow, rather than an unbounded wait, which looks like a hang. waitForGPU is the polling version, for a meeting transcription nobody is watching.
// Only the meeting path used to ask at all. Five of the eleven dictations in the six days to 2026-09-13 walked onto a card the embedding server still held, died with ErrorOutOfDeviceMemory, and were redone on the CPU at 12.9 to 20.7 seconds each.
func releaseTheCard() bool {
	if gpuReleaser == nil {
		return true
	}
	return gpuReleaser()
}

// gpuFailureMarks are what whisper.cpp's Vulkan backend prints as it runs out of card memory, and the crash that follows it, matched case-insensitively against everything the child printed.
var gpuFailureMarks = []string{"erroroutofdevicememory", "allocatememory", "out of memory", "segmentation fault"}

// gpuOutOfMemory reports whether a finished whisper run failed because the card had no memory for it, which a run on the CPU would not hit. Input: the run's context, the error exec returned, and everything the child printed on both streams. Output: true when the child was killed by a signal or named one of gpuFailureMarks, false when the run succeeded and when it was the context that ended it.
// The exit status alone is not enough to tell. The run that lost a dictation at 18:30 on 2026-09-06 printed "ggml_vulkan: Device memory allocation of size 462323712 failed" and then took SIGSEGV, which is not a clean non-zero exit at all, so the child's own output is read as well.
// A context that is already done means a cancelled or timed-out run, and retrying that on the CPU would only fail again more slowly.
func gpuOutOfMemory(ctx context.Context, err error, output string) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() < 0 {
		return true
	}
	low := strings.ToLower(output + " " + err.Error())
	for _, mark := range gpuFailureMarks {
		if strings.Contains(low, mark) {
			return true
		}
	}
	return false
}

// whisperGPUDevice names the Vulkan device whisper.cpp was told to decode on, for the log line of a run that died on it. Input: none, it reads the config. Output: a phrase like "vulkan device 1", or "vulkan device 0 (whisper.cpp's default)" when no device is configured.
func whisperGPUDevice() string {
	if d := config.LoadConfig().Transcribe.GPUDevice; d > 0 {
		return "vulkan device " + strconv.Itoa(d)
	}
	return "vulkan device 0 (whisper.cpp's default)"
}

// GPURun serializes whisper.cpp runs against each other. whisper-medium takes about 2.2 GB of GPU memory, and this machine's card has 4 GB with a share of it already spoken for, so the two streams of a call cannot be decoded at the same time — the second would fail to allocate. Running them one after the other costs nothing, because on the GPU the card is the bottleneck rather than the number of streams.
// Exported because a dictation runs its own whisper-cli outside this package (internal/ipc.Dictation) and has to queue behind a meeting's decode rather than race it: three of the four dictations on 2026-09-05 died with ErrorOutOfDeviceMemory while a meeting was being transcribed.
var GPURun sync.Mutex

// gpuHeld is raised for the whole of a whisper run, so the card's other tenant knows not to climb back onto it. Asking the embedding server to yield once, before the decode, was not enough: client presence is refreshed by every authenticated IPC request and the desktop window polls continuously, so the server was respawned seconds into a decode — 12 of the 37 out-of-memory crashes in the six days to 2026-09-12 had an embedding server start within the minute before them, several within two seconds.
var gpuHeld atomic.Bool

// GPUBusy reports whether a whisper decode owns the GPU right now. The daemon hands this to the embedding engine, which then stays off the card until the decode lets go. Input: none. Output: true while any whisper run is in flight.
func GPUBusy() bool { return gpuHeld.Load() }

// gpuReleaser asks the GPU's other tenant to leave before a whisper run — the daemon points it at the embedding server's StopIfIdle. It reports whether the tenant is actually gone; false means someone is mid-conversation and their embeds win. Nil means there is nothing sharing the card.
var gpuReleaser func() bool

// SetGPUReleaser wires gpuReleaser; the daemon calls it once at startup.
func SetGPUReleaser(f func() bool) { gpuReleaser = f }

// gpuWaitInterval is how long a decode waits between asking the embedding server to yield. Transcription is a background job with nowhere to be; half a minute per ask is patience, not delay. A variable so the tests can wind it down.
var gpuWaitInterval = 30 * time.Second

// waitForGPU blocks until the embedding server has yielded the card, asking again every gpuWaitInterval. Transcription never takes the GPU out from under a live conversation and never falls back to a slower decode — it just waits its turn.
func waitForGPU(ctx context.Context) error {
	for gpuReleaser != nil && !gpuReleaser() {
		slog.Info("waiting for the GPU: the embedding server is pinned by a live conversation")
		select {
		case <-ctx.Done():
			return fmt.Errorf("gave up waiting for the GPU: %w", ctx.Err())
		case <-time.After(gpuWaitInterval):
		}
	}
	return nil
}
