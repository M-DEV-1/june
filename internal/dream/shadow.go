package dream

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"time"

	"june/internal/util"
)

// shadowTimeout bounds one shadow brain call. Generous: a local Q2_K quantised model at roughly 76 tokens/second on a long dream prompt can take minutes, and getting the full reasoning trace out is the whole point of running it.
const shadowTimeout = 10 * time.Minute

// ShadowLifecycle optionally starts and stops the server the night's shadow brain talks to. Both funcs are optional: a nil Start assumes the shadow's backend is already reachable (or that there is none to manage), and a nil Stop is a no-op. dream() calls Start once before the night's stages and defers Stop, so the server's lifetime is exactly one night's run.
type ShadowLifecycle struct {
	Start func(ctx context.Context) error
	Stop  func()
}

// shadowArgs builds the llama-server command-line arguments for serving modelPath on port. device, when non-empty, is the Vulkan device name (e.g. "Vulkan1") appended as -dev; empty leaves the choice to llama-server. A pinned device also gets --no-mmap, see util.MmapForDevice.
func shadowArgs(modelPath string, port int, device string) []string {
	args := []string{"-m", modelPath, "--host", "127.0.0.1", "--port", strconv.Itoa(port), "-c", "8192", "-ngl", "99"}
	if device != "" {
		args = append(args, "-dev", device)
	}
	return util.MmapForDevice(args)
}

// NewLlamaServerLifecycle builds a ShadowLifecycle around llama-server: Start spawns it on 127.0.0.1:port serving modelPath and blocks until it answers /health or 120 seconds pass; Stop SIGTERMs the child and escalates to SIGKILL after five seconds if it hasn't gone. -ngl 99 offloads every layer to the GPU, the same flag the embedding engine uses; -c 8192 matches the dream prompts' context needs. device, when non-empty, is passed as -dev to pin llama-server to a specific Vulkan device (e.g. "Vulkan1") instead of whichever one it picks by default; if llama-server exits during startup with a device pinned, Start tries once more without it, the same fallback the embedding server makes.
func NewLlamaServerLifecycle(binary, modelPath string, port int, device string) ShadowLifecycle {
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	var cmd *exec.Cmd
	var exited chan struct{}

	// launch spawns llama-server with args and waits for it to answer /health. died reports that the process exited on its own during startup, as opposed to timing out or being cancelled.
	launch := func(ctx context.Context, args []string) (died bool, err error) {
		c := exec.Command(binary, args...)
		c.Stdout = os.Stderr
		c.Stderr = os.Stderr
		c.SysProcAttr = util.ChildProcAttr()
		if err := c.Start(); err != nil {
			return false, fmt.Errorf("shadow lifecycle: start %s: %w", binary, err)
		}
		util.KillWithDaemon(c)
		done := make(chan struct{})
		go func() { c.Wait(); close(done) }()

		if err := util.WaitHealthy(ctx, baseURL, done, 120*time.Second, 100*time.Millisecond); err != nil {
			select {
			case <-done:
				died = true
			default:
			}
			util.KillChild(c, done)
			return died, fmt.Errorf("shadow lifecycle: %w", err)
		}
		slog.Info("dream shadow: llama-server started", "pid", c.Process.Pid, "url", baseURL)
		cmd, exited = c, done
		return false, nil
	}
	start := func(ctx context.Context) error {
		args := shadowArgs(modelPath, port, device)
		died, err := launch(ctx, args)
		if err == nil || !died {
			return err
		}
		withoutDevice, pinned := util.DropDeviceArg(args)
		if pinned == "" {
			return err
		}
		slog.Warn("dream shadow: llama-server would not start on its GPU device, starting it on whatever device it finds", "device", pinned)
		_, err = launch(ctx, withoutDevice)
		return err
	}
	stop := func() {
		util.KillChild(cmd, exited)
		cmd, exited = nil, nil
	}
	return ShadowLifecycle{Start: start, Stop: stop}
}
