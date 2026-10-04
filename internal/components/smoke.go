package components

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"june/internal/recorder"
	"june/internal/util"
)

// smokeTimeout bounds one smoke-test run. whisper decodes a whole 30-second window even for one second of audio, which with the medium model on a slow CPU takes most of a minute.
const smokeTimeout = 3 * time.Minute

// smokeWAV is the clip every audio smoke test runs on: one second of 16 kHz mono silence, written beside the program so its name reaches the program as plain ASCII whatever the data directory is called (see recorder.whisperArgv for why a non-ASCII path does not).
const smokeWAV = "june-smoke.wav"

// errNoCUDA is a CUDA whisper build that never got onto the card, which finishFeature answers by installing the CPU variant.
var errNoCUDA = errors.New("the CUDA build of whisper.cpp could not use this GPU")

// errCUDABlocked is a CUDA whisper build that ran, but without ggml-cuda.dll, on a PC where Smart App Control is on (cudaBlocked). It is not answered with the CPU build the way errNoCUDA is: the CPU build is as unsigned, and its own DLLs may be blocked too, so fetching it is left to the person. run marks the card as one the CUDA build does not run on, so the card's Try again sets up the CPU build.
var errCUDABlocked = appControlError{"Smart App Control, which is on, seems to have blocked the part of whisper.cpp that uses the graphics card; choose Try again to set up the build that runs on the processor instead, which Smart App Control may block as well"}

// cudaBlocked reads a run of the CUDA whisper build for ggml-cuda.dll having been refused while Smart App Control is on. A whisper.cpp build with its backends as separate DLLs loads each at run time and logs the ones that loaded ("load_backend: loaded CPU backend from …"; the build checked does so even for -h), but in a release build says nothing about one that did not, so this is the only sign of a block, which otherwise reads as a card the build could not use. A run that loaded the CPU backend and not the CUDA one had ggml-cuda.dll refused; with Smart App Control off, that is a driver without nvcuda.dll, the CPU build's case. A log without the CPU line says nothing either way and is not read as a block. Input: the run's output. Output: true for a block.
func cudaBlocked(log string) bool {
	l := strings.ToLower(log)
	return strings.Contains(l, "loaded cpu backend") && !strings.Contains(l, "loaded cuda backend") && smartAppControl() == "on"
}

// startError is a program that never ran: it could not be started, Windows' application control blocked it or a DLL of it (appControlError), or the Windows loader stopped it before its first line for want of a DLL or an instruction. whisper-cli's own imports (whisper.dll, ggml.dll, ggml-base.dll, the C runtime and OpenMP) are the same in the CUDA and the CPU build — ggml loads its CUDA backend later, at run time, and only that load needs the NVIDIA driver — so such a failure is never answered by fetching the CPU build, which would fail the same way after half a gigabyte more.
type startError struct{ error }

func (e startError) Unwrap() error { return e.error }

// smokeFeature checks that a feature just installed runs on this machine, the way the daemon will run it. Input: the job's context, the job's id for its events, the feature id and its parts. Output: nil, errNoCUDA (wrapped) for a CUDA build that fell back to the CPU, errCUDABlocked when it fell back because Smart App Control refused ggml-cuda.dll, or an error saying what failed.
func (s *Service) smokeFeature(ctx context.Context, jobID, id string, parts []part) error {
	byID := map[string]part{}
	for _, pt := range parts {
		byID[pt.ID] = pt
	}
	switch id {
	case "transcribe":
		model, ok := byID["whisper-medium"]
		if !ok {
			model = byID["whisper-small"]
		}
		return s.smokeWhisper(ctx, jobID, byID["whisper-bin"], model)
	case "speakers":
		return s.smokeSherpa(ctx, jobID, byID["sherpa-bin"], true)
	case "memory", "summaries":
		return s.smokePart(ctx, jobID, byID["llama-server"])
	}
	return nil
}

// smokePart checks a component installed on its own: that its program starts and prints its help or version, which is enough to know every DLL or shared library it links was found. A model has nothing to run, so it passes once its hash has.
func (s *Service) smokePart(ctx context.Context, jobID string, pt part) error {
	switch pt.ID {
	case "whisper-bin":
		log, err := s.runSmoke(ctx, jobID, pt, nil, "-h")
		if err == nil && pt.Art.Needs == "cuda" && cudaBlocked(log) {
			// A build whose -h loads the backends shows the block here, before the model is fetched; with any other, the feature's own test finds it.
			return errCUDABlocked
		}
		return err
	case "sherpa-bin":
		return s.smokeSherpa(ctx, jobID, pt, false)
	case "llama-server":
		_, err := s.runSmoke(ctx, jobID, pt, nil, "--version")
		return err
	}
	return nil
}

// smokeWhisper decodes the silent clip with the model just installed. For the CUDA build it also checks whisper put the model on the card: ggml loads its CUDA backend at run time and carries on on the CPU without a word when that fails, so a clean exit alone does not mean the GPU works. A run that reached the card and then failed (out of memory, with the local text server on it) still proves CUDA works, and the daemon's own runs retry such a failure on the CPU, so it passes. One that stayed on the CPU because Smart App Control refused ggml-cuda.dll is errCUDABlocked, not errNoCUDA.
func (s *Service) smokeWhisper(ctx context.Context, jobID string, bin, model part) error {
	if bin.Art == nil || model.Art == nil {
		return nil
	}
	dir := s.abs(bin.Art.Dest)
	if err := writeSilentWAV(filepath.Join(dir, smokeWAV)); err != nil {
		return err
	}
	defer os.Remove(filepath.Join(dir, smokeWAV))
	// Queued behind any meeting or dictation decode, the same as the daemon's own runs, so the test never allocates on the card beside one. A meeting's decode can hold the card for minutes, so the window is told the test is waiting rather than left on the last download step, and the wait ends with the daemon.
	if !recorder.GPURun.TryLock() {
		s.publish(jobID, "testing", map[string]any{"file": bin.Art.Main, "waiting": true}, false)
		if err := lockGPU(ctx); err != nil {
			return err
		}
	}
	defer recorder.GPURun.Unlock()
	log, err := s.runSmoke(ctx, jobID, bin, nil, "-m", model.Art.Main, "-f", smokeWAV, "-nt", "-l", "en")
	if bin.Art.Needs != "cuda" {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if strings.Contains(strings.ToLower(log), "whisper_backend_init_gpu: using cuda") {
		return nil
	}
	var notRun startError
	if errors.As(err, &notRun) {
		return err
	}
	if cudaBlocked(log) {
		return errCUDABlocked
	}
	if err == nil {
		err = errors.New("it ran on the CPU")
	}
	return fmt.Errorf("%w: %v", errNoCUDA, err)
}

// lockGPU takes recorder.GPURun, giving up when ctx ends. sync.Mutex has no wait that can be cancelled, so it is tried every half second.
func lockGPU(ctx context.Context) error {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for !recorder.GPURun.TryLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
	return nil
}

// smokeSherpa runs the diarizer: over the silent clip with both models when the feature is complete, which is the only way to make it load ONNX Runtime (its --help never does), or for its help alone when the binary was installed by itself.
// ONNX Runtime matters on Windows in particular: Windows 11 has its own, older onnxruntime.dll in System32, which the loader prefers to one on PATH, so the build's copy has to sit beside the program — and only a real run shows it does.
func (s *Service) smokeSherpa(ctx context.Context, jobID string, bin part, withModels bool) error {
	if bin.Art == nil {
		return nil
	}
	dir := s.abs(bin.Art.Dest)
	env := libPathEnv(filepath.Join(dir, "lib"))
	if !withModels {
		_, err := s.runSmoke(ctx, jobID, bin, env, "--help")
		return err
	}
	if err := writeSilentWAV(filepath.Join(dir, smokeWAV)); err != nil {
		return err
	}
	defer os.Remove(filepath.Join(dir, smokeWAV))
	_, err := s.runSmoke(ctx, jobID, bin, env, "--segmentation.pyannote-model=segmentation-3.0.onnx", "--embedding.model=wespeaker_en_voxceleb_CAM++.onnx", "--clustering.num-clusters=1", smokeWAV)
	return err
}

// libPathEnv is the environment with dir put first on the shared-library search path, the way internal/recorder runs the diarizer.
func libPathEnv(dir string) []string {
	v := "LD_LIBRARY_PATH"
	if runtime.GOOS == "windows" {
		v = "PATH"
	}
	val := dir
	if old := os.Getenv(v); old != "" {
		val += string(os.PathListSeparator) + old
	}
	return append(os.Environ(), v+"="+val)
}

// runSmoke runs a part's program in its own directory, with no console window and tied to the daemon's lifetime like every other child. Input: the context, the job the events belong to, the part, extra environment (nil for the daemon's) and the arguments. Output: everything the program printed, and an error naming the program and what went wrong when it did not exit 0.
func (s *Service) runSmoke(ctx context.Context, jobID string, pt part, env []string, args ...string) (string, error) {
	s.publish(jobID, "testing", map[string]any{"file": pt.Art.Main}, false)
	ctx, cancel := context.WithTimeout(ctx, smokeTimeout)
	defer cancel()
	bin := s.abs(pt.mainPath())
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = filepath.Dir(bin)
	cmd.Env = env
	var out tailBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.SysProcAttr = util.ChildProcAttr()
	if err := cmd.Start(); err != nil {
		if blocked, ok := appControlStart(pt.Art.Main, err); ok {
			return "", startError{blocked}
		}
		return "", startError{fmt.Errorf("%s would not start: %w", pt.Art.Main, err)}
	}
	util.KillWithDaemon(cmd)
	err := cmd.Wait()
	log := out.String()
	if err == nil {
		return log, nil
	}
	switch ctx.Err() {
	case context.DeadlineExceeded:
		return log, fmt.Errorf("%s did not finish within %s", pt.Art.Main, smokeTimeout)
	case context.Canceled:
		return log, ctx.Err()
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if blocked, ok := appControlExit(pt.Art.Main, exit.ExitCode()); ok {
			return log, startError{blocked}
		}
		reason, loader := exitReason(exit.ExitCode())
		err := fmt.Errorf("%s failed its test run: %s%s", pt.Art.Main, reason, lastLines(log, 3))
		if loader {
			return log, startError{err}
		}
		if smartAppControl() == "on" {
			// A DLL loaded at run time rather than imported, such as ggml's CPU backends, is dropped without a word when Smart App Control refuses it, and the program then fails in a way of its own whose exit code names no block.
			err = fmt.Errorf("%w. Smart App Control is on and may have blocked a file it loads as it runs", err)
		}
		return log, err
	}
	return log, fmt.Errorf("%s failed its test run: %w", pt.Art.Main, err)
}

// exitReason explains the Windows loader's own exit codes, which are what a build missing a DLL or the C runtime dies with before printing anything. Output: the explanation, and true when the code is one of the loader's, so the program never ran.
func exitReason(code int) (string, bool) {
	switch uint32(code) {
	case 0xC0000135:
		// June ships the runtime in crt\ for exactly this, so it only happens when that copy is incomplete; the redistributable is what a person can still do about it.
		return "a DLL it needs is missing; installing Microsoft's Visual C++ Redistributable (x64) usually fixes this", true
	case 0xC0000139:
		return "a DLL it loaded is a different version from the one it was built against", true
	case 0xC000007B:
		return "a DLL it loaded is built for another kind of processor", true
	case 0xC000001D:
		return "this processor lacks an instruction the build needs", true
	}
	return fmt.Sprintf("exit status %d", code), false
}

// lastLines is the end of a program's output, for an error a person reads.
func lastLines(log string, n int) string {
	lines := strings.Split(strings.TrimSpace(log), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	tail := strings.TrimSpace(strings.Join(lines, " / "))
	if tail == "" {
		return ""
	}
	return ": " + tail
}

// tailBuffer keeps the last 64 KB written to it, which is all of a smoke test's output that is ever read.
type tailBuffer struct{ b bytes.Buffer }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b.Write(p)
	if over := t.b.Len() - 64<<10; over > 0 {
		t.b.Next(over)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return t.b.String() }

// writeSilentWAV writes one second of 16 kHz mono 16-bit silence.
func writeSilentWAV(path string) error {
	const rate, n = 16000, 16000
	h := make([]byte, 44)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], 36+n*2)
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1)
	binary.LittleEndian.PutUint16(h[22:], 1)
	binary.LittleEndian.PutUint32(h[24:], rate)
	binary.LittleEndian.PutUint32(h[28:], rate*2)
	binary.LittleEndian.PutUint16(h[32:], 2)
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], n*2)
	return os.WriteFile(path, append(h, make([]byte, n*2)...), 0o600)
}
