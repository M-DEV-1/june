package recorder

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"

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
var gpuRun sync.Mutex
