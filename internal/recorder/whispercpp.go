package recorder

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"june/internal/config"
	"june/internal/util"
)

// The transcription engine is whisper-medium (whisper-small on a machine with no GPU it can use, see WhisperModelName) through a whisper.cpp build, which decodes on the GPU — through its Vulkan backend on Linux, through CUDA on Windows, where whisper.cpp publishes no Vulkan build — and falls back to the CPU on its own when no GPU is there. It is installed into <data dir>/whispercpp: the whisper-cli binary and the ggml model beside it.
// whisper-cli has to be told which model to load, so the call site adds a couple of flags on top of the shared decoder thresholds.
const (
	whisperCPPBinaryName = "whisper-cli"
	// whisperCPPModelName is the model loaded when the config names none, which is every install made by hand before the config could name one.
	whisperCPPModelName = "ggml-medium.bin"
	// whisperVADModelName is the Silero voice-activity model whisper.cpp loads with --vad. It is optional: without it the decoder is handed the whole stream, as it was before.
	whisperVADModelName = "ggml-silero-v6.2.0.bin"
)

// WhisperModelName is the whisper.cpp model file looked for beside whisper-cli: transcribe.model from the config, or ggml-medium.bin when it names none. A machine with no GPU whisper can use is set up with ggml-small.bin instead, because medium on the CPU takes several seconds per dictation. Only the file's name is taken from the config, so a value with a directory in it still names a file beside whisper-cli.
func WhisperModelName() string {
	if m := filepath.Base(config.LoadConfig().Transcribe.Model); m != "." && m != string(filepath.Separator) {
		return m
	}
	return whisperCPPModelName
}

// WhisperCPPBinary returns the path to the whisper.cpp build: $JUNE_WHISPER_CPP if set, otherwise <dataDir>/whispercpp/whisper-cli, but only when the model is present beside it. The error names the exact path of each missing file, which is also what june doctor prints.
func WhisperCPPBinary(dataDir string) (string, error) {
	bin := whisperCPPPath(dataDir)
	name := WhisperModelName()
	model := filepath.Join(filepath.Dir(bin), name)
	if info, err := os.Stat(bin); err != nil || info.IsDir() {
		return "", fmt.Errorf("no whisper-cli at %s, and it needs %s beside it", bin, model)
	}
	if _, err := os.Stat(model); err != nil {
		return "", fmt.Errorf("no %s at %s, beside whisper-cli", name, model)
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
	name := whisperCPPBinaryName
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(dataDir, "whispercpp", name)
}

// whisperCPPArgs returns the flags that point whisper-cli at its model and its GPU, or nil when no model sits beside the binary — which is how a test's bare stub script gets run without flags it would not understand.
// The GPU number matters on a laptop with two of them: whisper.cpp counts every Vulkan device it can see and defaults to the first, which on this machine is the integrated graphics — several times slower than the discrete card sitting next to it. The number is per-machine, so it is configured rather than guessed.
// -dev N is how this build takes it (whisper-cli --help lists it beside -ng), which is why nothing here sets GGML_VK_VISIBLE_DEVICES: that variable hides devices and renumbers the ones left, so it changes the meaning of every number rather than naming one.
// Which number is the discrete card cannot be worked out from here. The order is whatever the Vulkan loader enumerates, and it moves with a driver update, a BIOS graphics setting or an external card being plugged in, so "device 1 is the NVIDIA" is a fact about this machine today and belongs in the config. What the code does instead of trusting the order is survive a bad guess: a decode that dies on the chosen device is redone on the CPU (see RunWhisper).
func whisperCPPArgs(bin string) []string {
	model := filepath.Join(filepath.Dir(bin), WhisperModelName())
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

// RunWhisper runs one whisper-cli command and, when that run dies for want of GPU memory, runs the same command once more with the GPU switched off rather than losing the recording. Input: the context, the whisper-cli binary and the arguments of the GPU run. Output: the stdout, the stderr cut back by quietLog, and the error of whichever run answered — the CPU one whenever there was a retry.
// Both attempts happen inside the caller's GPU turn: transcribeWAV and internal/ipc's Dictation.finish each hold GPURun across their whole decode, so the retry cannot race the local text server or the other stream any more than the first attempt could. The audio file is the caller's too, and it deletes it only once this returns.
// whisper-cli runs without -np whoever asked for it. -np silences whisper's own log, and that log is the only place whisper says it found no GPU at the configured number, or that CUDA had no memory left for a buffer, so with it on the warning below could never fire (checked against the installed CUDA build on 2026-10-03: with -np its stderr is the backend's device list and nothing more). stdout is the same either way.
func RunWhisper(ctx context.Context, bin string, args []string) (stdout, stderr string, err error) {
	dir, args := whisperArgv(withoutNoPrints(args))
	if dir != "" {
		// A relative binary would be looked for under dir once the child starts there.
		if abs, err := filepath.Abs(bin); err == nil {
			bin = abs
		}
	}
	// Held across both attempts: the CPU retry is what the first attempt's crash cost us, and letting the text server back on the card halfway would only set up the next crash.
	gpuHeld.Store(true)
	defer gpuHeld.Store(false)
	// Claim the card first, then ask its other tenant to leave: gpuHeld is what stops the text server climbing straight back on, so raising it before the ask is what makes the ask stick.
	releaseTheCard()
	out, log, err := runWhisperIn(ctx, dir, bin, args)
	// Read whatever the run's outcome: a run that found no GPU and then failed for some other reason was still on the wrong device.
	warnIfNoGPU(log)
	if !gpuOutOfMemory(ctx, err, out+log) {
		return out, quietLog(log), err
	}
	slog.Warn("whisper died on the GPU, transcribing the same audio again on the CPU", "device", whisperGPUDevice(), "error", err, "stderr", quietLog(log))
	// -ng is whisper-cli's own switch for decoding without a GPU, and it goes on the end so it wins whatever the first attempt was given.
	out, log, err = runWhisperIn(ctx, dir, bin, append(append([]string{}, args...), "-ng"))
	return out, quietLog(log), err
}

// withoutNoPrints returns args with whisper-cli's -np (--no-prints) taken out, and a --prompt's text left alone whatever it says. args itself is not changed.
func withoutNoPrints(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--prompt" && i+1 < len(args) {
			out = append(out, args[i], args[i+1])
			i++
			continue
		}
		if args[i] == "-np" || args[i] == "--no-prints" {
			continue
		}
		out = append(out, args[i])
	}
	return out
}

// quietLog is whisper's stderr with the routine lines of its own log taken out: the model's and state's sizes, the VAD's figures, the system info and the timings, about 6 KB a run. What is left is what -np used to leave, the backend's device list and the audio read, plus every line of whisper's log that reports a failure, so an error a person reads (the "Meeting transcription failed" notice, a dictation's error) names what went wrong rather than how the model loaded. Input: the whole stderr. Output: the lines kept, in order.
// whisper's log lines all start with the whisper_ function that wrote them, and system_info and "main: processing" are whisper-cli's own lines that -np turned off.
func quietLog(log string) string {
	var kept []string
	for _, line := range strings.Split(log, "\n") {
		line = strings.TrimRight(line, "\r")
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		low := strings.ToLower(t)
		routine := strings.HasPrefix(t, "whisper_") || strings.HasPrefix(t, "system_info:") || strings.HasPrefix(t, "main: processing")
		if routine && !strings.Contains(low, "fail") && !strings.Contains(low, "error") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// whisperArgv makes a whisper-cli argument list survive the way whisper-cli reads it on Windows. Its main() takes argv in the ANSI code page (examples/cli/cli.cpp says so beside its SetConsoleOutputCP call, on master as of 2026-10-03), then opens files through narrow-string calls and reads --prompt as UTF-8. A path holding a character that code page lacks — a user name in another script puts one in every temp WAV and in the model's path — arrives with '?' in it and cannot be opened, and a prompt outside ASCII arrives as code-page bytes rather than the words sent. So paths go over as their 8.3 short forms and the prompt keeps only what survives.
// A volume with short names turned off (NtfsDisable8dot3NameCreation, which is 1 on the machine this was written on) leaves a path as it was. Its non-ASCII part is then usually the user's own folder, which the model and the temp WAV share, so whisper-cli is started inside it and handed both as ASCII paths relative to it; the working directory goes to CreateProcess as UTF-16, so it needs no code page.
// Input: the arguments as built. Output: the directory to start whisper-cli in ("" for the daemon's own, always outside Windows) and a new argument slice, args left as it was.
func whisperArgv(args []string) (dir string, out []string) {
	out = append([]string{}, args...)
	var paths []int
	for i := 0; i+1 < len(out); i++ {
		switch out[i] {
		case "-f", "--file", "-m", "--model", "-vm", "--vad-model":
			i++
			paths = append(paths, i)
		case "--prompt":
			i++
			out[i] = util.ANSIText(out[i])
		}
	}
	vals := make([]string, len(paths))
	for j, i := range paths {
		vals[j] = out[i]
	}
	dir, vals = childPaths(vals)
	for j, i := range paths {
		out[i] = vals[j]
	}
	return dir, out
}

// childPaths is whisperArgv's rule for file paths, shared with the diarizer, whose sherpa-onnx main() reads argv the same way and then decodes each path as UTF-8, so even a character the code page has ("José" under 1252) arrives as bytes it cannot open. Each path goes over as its absolute 8.3 short form, and one that is still not ASCII goes as an ASCII path relative to the directory the first such path is in, which the program is started in.
// Input: the paths. Output: the directory to start the program in ("" when no path needed one, and always outside Windows) and the paths to pass, in order.
func childPaths(paths []string) (dir string, out []string) {
	out = append([]string{}, paths...)
	if runtime.GOOS != "windows" {
		return "", out
	}
	for i := range out {
		// Absolute first, so a path that stays as it is still names the same file if the working directory moves below.
		if abs, err := filepath.Abs(out[i]); err == nil {
			out[i] = util.ShortPath(abs)
		}
		if isASCII(out[i]) {
			continue
		}
		if dir == "" {
			dir = filepath.Dir(out[i])
		}
		if rel, err := filepath.Rel(dir, out[i]); err == nil && isASCII(rel) {
			out[i] = rel
		}
	}
	return dir, out
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// runWhisperIn runs whisper-cli through runChild, started in dir. An empty dir, which is every run but the one whisperArgv describes, keeps the daemon's own working directory.
func runWhisperIn(ctx context.Context, dir, bin string, args []string) (stdout, stderr string, err error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	return runChild(cmd)
}

// releaseTheCard asks the GPU's other tenant to yield before a whisper run, and reports whether it went. It asks once and does not poll, because this runs on the dictation path too and somebody is waiting on that: a tenant that will not move immediately costs the run a CPU retry, which is slow, rather than an unbounded wait, which looks like a hang. waitForGPU is the polling version, for a meeting transcription nobody is watching.
// Only the meeting path used to ask at all. Five of the eleven dictations in the six days to 2026-09-13 walked onto a card the embedding server (the tenant then) still held, died with ErrorOutOfDeviceMemory, and were redone on the CPU at 12.9 to 20.7 seconds each.
func releaseTheCard() bool {
	if gpuReleaser == nil {
		return true
	}
	return gpuReleaser()
}

// gpuFailureMarks are what whisper.cpp's GPU backends print as they run out of card memory or fail on the card, and the crash that follows, matched case-insensitively against everything the child printed. The Vulkan ones were measured on Linux; ggml's CUDA backend on Windows says "cudaMalloc failed: out of memory" and, for any other failure on the card, "CUDA error".
var gpuFailureMarks = []string{"erroroutofdevicememory", "allocatememory", "out of memory", "segmentation fault", "cuda error"}

// warnIfNoGPU logs a run that decoded on the CPU because whisper.cpp could see no GPU at the configured number. That run succeeds, only several times slower (28.6 s against about 5 s for a 13 s dictation on a 3050), and nothing else says why: whisper.cpp prints "no GPU found" and carries on. It matters most on Windows, where the CUDA build counts only the NVIDIA card, so a gpu_device carried over from a Vulkan machine, where the integrated GPU is device 0, names nothing.
// Input: whisper's whole stderr, which only has these lines when it ran without -np. "no GPU found" alone is not the sign: the Silero VAD model is always loaded with the GPU off, so a run with --vad prints it on the right card too (seen on the installed build with -dev 0 on 2026-10-03). The model's own GPU says "whisper_backend_init_gpu: using CUDA0 backend" when it found one, so the warning is for a run that names no GPU it is using. Each "found GPU device" line is one GPU the build counted, which is the range gpu_device has to fall in.
func warnIfNoGPU(log string) {
	d := config.LoadConfig().Transcribe.GPUDevice
	low := strings.ToLower(log)
	if d <= 0 || !strings.Contains(low, "no gpu found") || strings.Contains(low, "whisper_backend_init_gpu: using ") {
		return
	}
	slog.Warn("whisper decoded on the CPU: the configured transcribe.gpu_device names no GPU this whisper.cpp build can see", "gpu_device", d, "gpus_seen", strings.Count(low, "whisper_backend_init_gpu: found gpu device"))
}

// gpuOutOfMemory reports whether a finished whisper run failed because the card had no memory for it, which a run on the CPU would not hit. Input: the run's context, the error exec returned, and everything the child printed on both streams. Output: true when the child was killed by a signal or named one of gpuFailureMarks, false when the run succeeded and when it was the context that ended it.
// The exit status alone is not enough to tell. The run that lost a dictation at 18:30 on 2026-09-06 printed "ggml_vulkan: Device memory allocation of size 462323712 failed" and then took SIGSEGV, which is not a clean non-zero exit at all, so the child's own output is read as well.
// A context that is already done means a cancelled or timed-out run, and retrying that on the CPU would only fail again more slowly.
func gpuOutOfMemory(ctx context.Context, err error, output string) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && crashed(exit.ExitCode()) {
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

// crashed reports whether an exit code is a crash rather than an exit the program chose. Linux reports a death by signal, the SIGSEGV of the 2026-09-06 run, as -1. Windows has no signals: a crash ends the process with the NTSTATUS of the exception, 0xC0000005 for an access violation, 0xC0000409 for the fail-fast abort() makes and 0xC000001D for an illegal instruction, and Go reports that as a large positive number, so the -1 test alone never saw a Windows crash and never gave it the CPU retry. Every NTSTATUS error code has its top two bits set.
func crashed(code int) bool {
	return code < 0 || uint32(code) >= 0xC0000000
}

// whisperGPUDevice names the GPU whisper.cpp was told to decode on, numbered as its backend numbers them (Vulkan devices on Linux, CUDA devices on Windows), for the log line of a run that died on it. Input: none, it reads the config. Output: a phrase like "gpu device 1", or "gpu device 0 (whisper.cpp's default)" when no device is configured.
func whisperGPUDevice() string {
	if d := config.LoadConfig().Transcribe.GPUDevice; d > 0 {
		return "gpu device " + strconv.Itoa(d)
	}
	return "gpu device 0 (whisper.cpp's default)"
}

// GPURun serializes whisper.cpp runs against each other. whisper-medium takes about 2.2 GB of GPU memory, and this machine's card has 4 GB with a share of it already spoken for, so the two streams of a call cannot be decoded at the same time — the second would fail to allocate. Running them one after the other costs nothing, because on the GPU the card is the bottleneck rather than the number of streams.
// Exported because a dictation runs its own whisper-cli outside this package (internal/ipc.Dictation) and has to queue behind a meeting's decode rather than race it: three of the four dictations on 2026-09-05 died with ErrorOutOfDeviceMemory while a meeting was being transcribed.
var GPURun sync.Mutex

// gpuHeld is raised for the whole of a whisper run, so the card's other tenant knows not to climb back onto it. Asking that tenant to yield once, before the decode, was not enough when it was still the embedding server: client presence is refreshed by every authenticated IPC request and the desktop window polls continuously, so the server was respawned seconds into a decode — 12 of the 37 out-of-memory crashes in the six days to 2026-09-12 had an embedding server start within the minute before them, several within two seconds. The tenant is the local text server now (cmd/daemon.go leaves the embedder resident, since it fits beside whisper), and it is spawned on demand the same way.
var gpuHeld atomic.Bool

// GPUBusy reports whether a whisper decode owns the GPU right now. The daemon hands this to the local text engine (embed.TextEngine.SetGPUGate), which then stays off the card until the decode lets go. Input: none. Output: true while any whisper run is in flight.
func GPUBusy() bool { return gpuHeld.Load() }

// gpuReleaser asks the GPU's other tenant to leave before a whisper run — the daemon points it at the local text server's StopIfIdle. It reports whether the tenant is actually gone; false means it will not go yet. The text server never refuses, since nobody waits on a working-state derive, so false is only ever a releaser that can say no. Nil means there is nothing sharing the card.
var gpuReleaser func() bool

// SetGPUReleaser wires gpuReleaser; the daemon calls it once at startup.
func SetGPUReleaser(f func() bool) { gpuReleaser = f }

// gpuWaitInterval is how long a decode waits between asking the card's other tenant to yield. Transcription is a background job with nowhere to be; half a minute per ask is patience, not delay. A variable so the tests can wind it down.
var gpuWaitInterval = 30 * time.Second

// waitForGPU blocks until the card's other tenant has yielded, asking again every gpuWaitInterval. Transcription never takes the GPU from a tenant that has said it is busy and never falls back to a slower decode — it just waits its turn. With the daemon's releaser today the first ask always succeeds and this returns at once.
func waitForGPU(ctx context.Context) error {
	for gpuReleaser != nil && !gpuReleaser() {
		slog.Info("waiting for the GPU: the card's other tenant has not let go of it yet")
		select {
		case <-ctx.Done():
			return fmt.Errorf("gave up waiting for the GPU: %w", ctx.Err())
		case <-time.After(gpuWaitInterval):
		}
	}
	return nil
}
