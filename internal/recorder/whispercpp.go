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
	"time"

	"ora/internal/config"
)

// The transcription engine is whisper-medium through a whisper.cpp build, which decodes on the GPU through its Vulkan backend and falls back to the CPU on its own when no GPU is there. It is installed into <data dir>/whispercpp: the whisper-cli binary and the ggml model beside it.
// whisper-cli has to be told which model to load, so the call site adds a couple of flags on top of the shared decoder thresholds.
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
		return "", fmt.Errorf("no whisper.cpp build at %s: put whisper-cli and its model there", bin)
	}
	if _, err := os.Stat(filepath.Join(dir, whisperCPPModelName)); err != nil {
		return "", fmt.Errorf("the whisper.cpp build at %s is missing %s beside it", bin, whisperCPPModelName)
	}
	return bin, nil
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
	args := []string{"-m", model}
	if d := config.LoadConfig().Transcribe.GPUDevice; d > 0 {
		args = append(args, "-dev", strconv.Itoa(d))
	}
	return args
}

// RunWhisper runs one whisper-cli command and, when that run dies for want of GPU memory, runs the same command once more with the GPU switched off rather than losing the recording. Input: the context, the whisper-cli binary and the arguments of the GPU run. Output: the stdout, stderr and error of whichever run answered — the CPU one whenever there was a retry.
// Both attempts happen inside the caller's GPU turn: transcribeWAV and internal/ipc's Dictation.finish each hold GPURun across their whole decode, so the retry cannot race the embedding server or the other stream any more than the first attempt could. The audio file is the caller's too, and it deletes it only once this returns.
func RunWhisper(ctx context.Context, bin string, args []string) (stdout, stderr string, err error) {
	out, errOut, err := run(ctx, bin, args)
	if !gpuOutOfMemory(ctx, err, out+errOut) {
		return out, errOut, err
	}
	slog.Warn("whisper died on the GPU, transcribing the same audio again on the CPU", "device", whisperGPUDevice(), "error", err, "stderr", strings.TrimSpace(errOut))
	// -ng is whisper-cli's own switch for decoding without a GPU, and it goes on the end so it wins whatever the first attempt was given.
	return run(ctx, bin, append(append([]string{}, args...), "-ng"))
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
