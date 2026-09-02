package dream

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// shadowTimeout bounds one shadow brain call. Generous: a local Q2_K quantised model at roughly 76 tokens/second on a long dream prompt can take minutes, and getting the full reasoning trace out is the whole point of running it.
const shadowTimeout = 10 * time.Minute

// ShadowLifecycle optionally starts and stops the server the night's shadow brain talks to. Both funcs are optional: a nil Start assumes the shadow's backend is already reachable (or that there is none to manage), and a nil Stop is a no-op. dream() calls Start once before the night's stages and defers Stop, so the server's lifetime is exactly one night's run.
type ShadowLifecycle struct {
	Start func(ctx context.Context) error
	Stop  func()
}

// shadowArgs builds the llama-server command-line arguments for serving modelPath on port. device, when non-empty, is the Vulkan device name (e.g. "Vulkan1") appended as -dev; empty leaves the choice to llama-server.
func shadowArgs(modelPath string, port int, device string) []string {
	args := []string{"-m", modelPath, "--host", "127.0.0.1", "--port", strconv.Itoa(port), "-c", "8192", "-ngl", "99"}
	if device != "" {
		args = append(args, "-dev", device)
	}
	return args
}

// NewLlamaServerLifecycle builds a ShadowLifecycle around llama-server: Start spawns it on 127.0.0.1:port serving modelPath and blocks until it answers /health or 120 seconds pass; Stop SIGTERMs the child and escalates to SIGKILL after five seconds if it hasn't gone. -ngl 99 offloads every layer to the GPU, the same flag the embedding engine uses; -c 8192 matches the dream prompts' context needs. device, when non-empty, is passed as -dev to pin llama-server to a specific Vulkan device (e.g. "Vulkan1") instead of whichever one it picks by default.
func NewLlamaServerLifecycle(binary, modelPath string, port int, device string) ShadowLifecycle {
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	var cmd *exec.Cmd
	var exited chan struct{}

	start := func(ctx context.Context) error {
		c := exec.Command(binary, shadowArgs(modelPath, port, device)...)
		c.Stdout = os.Stderr
		c.Stderr = os.Stderr
		c.SysProcAttr = childProcAttr()
		if err := c.Start(); err != nil {
			return fmt.Errorf("shadow lifecycle: start %s: %w", binary, err)
		}
		done := make(chan struct{})
		go func() { c.Wait(); close(done) }()

		if err := waitHealthy(ctx, baseURL, done, 120*time.Second); err != nil {
			killShadowChild(c, done)
			return err
		}
		slog.Info("dream shadow: llama-server started", "pid", c.Process.Pid, "url", baseURL)
		cmd, exited = c, done
		return nil
	}
	stop := func() {
		killShadowChild(cmd, exited)
		cmd, exited = nil, nil
	}
	return ShadowLifecycle{Start: start, Stop: stop}
}

// waitHealthy polls baseURL's /health until it answers 200, the child exits, ctx is cancelled, or timeout passes.
func waitHealthy(ctx context.Context, baseURL string, exited chan struct{}, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		select {
		case <-exited:
			return fmt.Errorf("shadow lifecycle: server exited during startup")
		case <-ctx.Done():
			return fmt.Errorf("shadow lifecycle: %w", ctx.Err())
		default:
		}
		if req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/health", nil); err == nil {
			if resp, err := client.Do(req); err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("shadow lifecycle: server did not become ready at %s within %v", baseURL, timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// killShadowChild SIGTERMs a spawned child and waits for it to go, escalating to SIGKILL after five seconds. A nil cmd is a no-op.
func killShadowChild(cmd *exec.Cmd, exited chan struct{}) {
	if cmd == nil {
		return
	}
	cmd.Process.Signal(os.Interrupt)
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		<-exited
	}
}
