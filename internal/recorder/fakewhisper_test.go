package recorder

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestMain lets the test binary stand in for whisper-cli, so the whisper tests run the same on Linux and Windows. Started with JUNE_TEST_WHISPER=1 it acts as the fake and exits before the testing package reads the whisper flags it was handed.
func TestMain(m *testing.M) {
	if os.Getenv("JUNE_TEST_WHISPER") == "1" {
		os.Exit(fakeWhisperMain(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeWhisperMain is the fake whisper-cli, steered by environment variables the test sets:
// FAKE_WHISPER_ARGLOG appends this run's arguments as one line to that file.
// FAKE_WHISPER_BUSY marks that directory busy for 300 ms, and when another run had already marked it, prints OVERLAP to stderr and exits 4.
// FAKE_WHISPER_RAN is a file touched on every run; with FAKE_WHISPER_DIE=once the run dies when the file was not there yet, and with FAKE_WHISPER_DIE=always every run dies.
// A run that dies prints the Vulkan out-of-memory lines whisper.cpp printed on 2026-09-06 and exits 3. Any other run prints FAKE_WHISPER_STDOUT and FAKE_WHISPER_STDERR and exits 0.
// Input: the arguments whisper-cli was given. Output: the exit code.
func fakeWhisperMain(args []string) int {
	if log := os.Getenv("FAKE_WHISPER_ARGLOG"); log != "" {
		f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return 9
		}
		fmt.Fprintln(f, strings.Join(args, " "))
		f.Close()
	}
	if dir := os.Getenv("FAKE_WHISPER_BUSY"); dir != "" {
		busy := filepath.Join(dir, "running")
		_, err := os.Stat(busy)
		overlap := err == nil
		os.WriteFile(busy, nil, 0o644)
		time.Sleep(300 * time.Millisecond)
		os.Remove(busy)
		if overlap {
			fmt.Fprintln(os.Stderr, "OVERLAP")
			return 4
		}
	}
	firstRun := true
	if ran := os.Getenv("FAKE_WHISPER_RAN"); ran != "" {
		_, err := os.Stat(ran)
		firstRun = os.IsNotExist(err)
		os.WriteFile(ran, nil, 0o644)
	}
	if die := os.Getenv("FAKE_WHISPER_DIE"); die == "always" || (die == "once" && firstRun) {
		fmt.Fprintln(os.Stderr, "ggml_vulkan: Device memory allocation of size 462323712 failed.")
		fmt.Fprintln(os.Stderr, "ggml_vulkan: vk::Device::allocateMemory: ErrorOutOfDeviceMemory")
		return 3
	}
	fmt.Fprintln(os.Stdout, os.Getenv("FAKE_WHISPER_STDOUT"))
	fmt.Fprintln(os.Stderr, os.Getenv("FAKE_WHISPER_STDERR"))
	return 0
}

// fakeWhisperBin copies the test binary into a fresh directory under whisper-cli's own name and switches it into the fake. With model set, a stand-in ggml model goes beside it, so whisperCPPArgs adds the model flags and the run takes the GPU path.
// Input: whether to put a model beside it. Output: the binary's path.
func fakeWhisperBin(t *testing.T, model bool) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("find the test binary: %v", err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, whisperCPPBinaryName)
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	src, err := os.Open(exe)
	if err != nil {
		t.Fatalf("open the test binary: %v", err)
	}
	defer src.Close()
	dst, err := os.OpenFile(bin, os.O_CREATE|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatalf("create fake whisper: %v", err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatalf("copy the test binary: %v", err)
	}
	if err := dst.Close(); err != nil {
		t.Fatalf("close fake whisper: %v", err)
	}
	if model {
		if err := os.WriteFile(filepath.Join(dir, whisperCPPModelName), []byte("not a model"), 0o644); err != nil {
			t.Fatalf("write fake model: %v", err)
		}
	}
	t.Setenv("JUNE_TEST_WHISPER", "1")
	return bin
}
