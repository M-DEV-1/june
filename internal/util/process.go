package util

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// WaitHealthy polls baseURL's /health until it answers 200. Input: a context that stops the wait, the server's base URL, a channel closed when the child exits, how long to wait in all, and how long to sleep between polls. Output: nil once healthy, or an error saying the child exited, the context ended, or the timeout passed.
func WaitHealthy(ctx context.Context, baseURL string, exited <-chan struct{}, timeout, poll time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		select {
		case <-exited:
			return fmt.Errorf("server exited during startup")
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/health", nil)
		if err != nil {
			return fmt.Errorf("build health request: %w", err)
		}
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("server did not become ready at %s within %v", baseURL, timeout)
		}
		time.Sleep(poll)
	}
}

// fullOffload is the smallest -ngl value that counts as putting every layer on the GPU. Every launch site passes 99, which is more layers than any model June runs has.
const fullOffload = 99

// MmapForDevice returns a llama-server argument list with "--no-mmap" present exactly when the server runs fully on a GPU device: "-dev" or "--device" given and "-ngl" at least 99 or "all". Input: the arguments. Output: a copy. With the weights in VRAM, mmap only keeps the model file's pages resident in RAM a second time; on the CPU the flag is removed so the kernel can still drop the mapped pages under memory pressure.
func MmapForDevice(args []string) []string {
	out := make([]string, 0, len(args)+1)
	hasDevice, offloaded := false, false
	for i, a := range args {
		switch a {
		case "--no-mmap":
			continue
		case "-dev", "--device":
			hasDevice = true
		case "-ngl", "--gpu-layers", "--n-gpu-layers":
			if i+1 < len(args) {
				n, err := strconv.Atoi(args[i+1])
				offloaded = args[i+1] == "all" || (err == nil && n >= fullOffload)
			}
		}
		out = append(out, a)
	}
	if hasDevice && offloaded {
		out = append(out, "--no-mmap")
	}
	return out
}

// DropDeviceArg removes the "-dev <name>" or "--device <name>" pair from a llama-server argument list, and "--no-mmap" with it since the server then runs on the CPU. Input: the arguments. Output: a copy without the pair and the device name removed, or the arguments unchanged and an empty name when no device is pinned.
func DropDeviceArg(args []string) (rest []string, device string) {
	for i, a := range args {
		if (a == "--device" || a == "-dev") && i+1 < len(args) {
			return MmapForDevice(append(append([]string{}, args[:i]...), args[i+2:]...)), args[i+1]
		}
	}
	return args, ""
}

// KillChild interrupts a spawned child and waits for it to go, escalating to SIGKILL after five seconds. Input: the command and the channel closed when it exits. A nil cmd is a no-op.
// It can block for five seconds, so call it without holding a lock.
func KillChild(cmd *exec.Cmd, exited <-chan struct{}) {
	if cmd == nil {
		return
	}
	// Windows cannot deliver an interrupt to another process, so a failed signal goes straight to the kill.
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		cmd.Process.Kill()
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		<-exited
	}
}
