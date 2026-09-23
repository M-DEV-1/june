package util

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// ChildProcAttr asks the kernel to SIGTERM a spawned server if the daemon dies. Without it a daemon killed with SIGKILL, which runs no shutdown path, leaves its llama-server children orphaned and holding their GPU memory.
func ChildProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}

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

// KillChild interrupts a spawned child and waits for it to go, escalating to SIGKILL after five seconds. Input: the command and the channel closed when it exits. A nil cmd is a no-op.
// It can block for five seconds, so call it without holding a lock.
func KillChild(cmd *exec.Cmd, exited <-chan struct{}) {
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
