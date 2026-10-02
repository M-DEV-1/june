package ipc

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// TestMain lets the test binary stand in for the command-line tools the daemon runs (agy, whisper-cli), so the tests that run them work the same on Linux and Windows. Started with JUNE_TEST_HELPER=1 it acts as the fake and exits before the testing package reads the arguments it was handed.
func TestMain(m *testing.M) {
	if os.Getenv("JUNE_TEST_HELPER") == "1" {
		os.Exit(fakeCommandMain(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeCommandMain is the fake tool, steered by environment variables the test sets:
// FAKE_LOG appends one line per run to that file: "wav-present" or "wav-gone" for whether the file named by the second argument exists, then the arguments.
// FAKE_RAN is a file touched on every run; with FAKE_DIE=once the run dies when the file was not there yet, and with FAKE_DIE=always every run dies. A run that dies prints the Vulkan out-of-memory lines whisper.cpp printed on 2026-09-06 and exits 3.
// Any other run prints FAKE_STDOUT and FAKE_STDERR and exits with FAKE_EXIT, 0 when unset.
// Input: the arguments the tool was given. Output: the exit code.
func fakeCommandMain(args []string) int {
	if log := os.Getenv("FAKE_LOG"); log != "" {
		state := "wav-gone"
		if len(args) > 1 {
			if _, err := os.Stat(args[1]); err == nil {
				state = "wav-present"
			}
		}
		f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return 9
		}
		fmt.Fprintln(f, state+" "+strings.Join(args, " "))
		f.Close()
	}
	firstRun := true
	if ran := os.Getenv("FAKE_RAN"); ran != "" {
		_, err := os.Stat(ran)
		firstRun = os.IsNotExist(err)
		os.WriteFile(ran, nil, 0o644)
	}
	if die := os.Getenv("FAKE_DIE"); die == "always" || (die == "once" && firstRun) {
		fmt.Fprintln(os.Stderr, "ggml_vulkan: Device memory allocation of size 462323712 failed.")
		fmt.Fprintln(os.Stderr, "ggml_vulkan: vk::Device::allocateMemory: ErrorOutOfDeviceMemory")
		return 3
	}
	fmt.Fprint(os.Stdout, os.Getenv("FAKE_STDOUT"))
	fmt.Fprint(os.Stderr, os.Getenv("FAKE_STDERR"))
	code, _ := strconv.Atoi(os.Getenv("FAKE_EXIT"))
	return code
}

// fakeCommand copies the test binary into dir under name, with .exe added on Windows, and switches it into the fake. Input: the directory and the tool's name. Output: the fake's path.
func fakeCommand(t *testing.T, dir, name string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("find the test binary: %v", err)
	}
	bin := filepath.Join(dir, name)
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
		t.Fatalf("create fake %s: %v", name, err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatalf("copy the test binary: %v", err)
	}
	if err := dst.Close(); err != nil {
		t.Fatalf("close fake %s: %v", name, err)
	}
	t.Setenv("JUNE_TEST_HELPER", "1")
	return bin
}
