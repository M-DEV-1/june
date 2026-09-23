package embed

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"
)

// startState is one in-flight spawn-and-load. err is written before done is closed, so every waiter that observes the close sees it.
type startState struct {
	done chan struct{}
	err  error
}

// serverProcess manages a child llama-server process lifecycle: lazy start, readiness polling, exit watching, idle shutdown, and graceful termination.
type serverProcess struct {
	name           string // "embed engine" or "text engine"
	logTag         string // "embedding server" or "local text server"
	binary         string
	args           []string
	baseURL        string
	idle           time.Duration
	startupTimeout time.Duration
	pollInterval   time.Duration

	mu      sync.Mutex
	cmd     *exec.Cmd
	exited  chan struct{}
	start   *startState
	lastUse time.Time
	closed  bool

	// shouldKeepAlive is an optional callback checked during idle sweeps (e.g. client presence pinning).
	shouldKeepAlive func() bool
}

func newServerProcess(name, logTag, binary string, args []string, baseURL string, idle, startupTimeout, pollInterval time.Duration) *serverProcess {
	return &serverProcess{
		name:           name,
		logTag:         logTag,
		binary:         binary,
		args:           args,
		baseURL:        baseURL,
		idle:           idle,
		startupTimeout: startupTimeout,
		pollInterval:   pollInterval,
	}
}

func (p *serverProcess) running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.aliveLocked()
}

func (p *serverProcess) aliveLocked() bool {
	if p.cmd == nil || p.cmd.Process == nil || p.exited == nil {
		return false
	}
	select {
	case <-p.exited:
		return false
	default:
		return true
	}
}

func (p *serverProcess) touchUse() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastUse = time.Now()
}

func (p *serverProcess) ensureUp(ctx context.Context) error {
	start, err := p.beginStart()
	if err != nil {
		return err
	}
	if start == nil {
		return nil
	}
	select {
	case <-start.done:
		return start.err
	case <-ctx.Done():
		return fmt.Errorf("%s: waiting for server: %w", p.name, ctx.Err())
	}
}

func (p *serverProcess) stopNow() bool {
	p.mu.Lock()
	cmd, exited := p.detachLocked()
	p.mu.Unlock()
	if cmd != nil {
		slog.Info("stopping the " + p.logTag + " to free its GPU memory")
	}
	killChild(cmd, exited)
	return true
}

func (p *serverProcess) beginStart() (*startState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil, fmt.Errorf("%s: closed", p.name)
	}
	if p.start != nil {
		return p.start, nil
	}
	if p.aliveLocked() {
		return nil, nil
	}

	cmd := exec.Command(p.binary, p.args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = childProcAttr()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%s: start %s: %w", p.name, p.binary, err)
	}

	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		close(exited)
	}()
	p.cmd, p.exited = cmd, exited
	p.lastUse = time.Now()
	slog.Info(p.logTag+" started", "pid", cmd.Process.Pid, "url", p.baseURL)

	start := &startState{done: make(chan struct{})}
	p.start = start
	go p.finishStart(start, exited)
	return start, nil
}

func (p *serverProcess) finishStart(start *startState, exited chan struct{}) {
	err := p.waitReady(exited)

	p.mu.Lock()
	var cmd *exec.Cmd
	var dead chan struct{}
	if err != nil {
		cmd, dead = p.detachLocked()
		p.dropDeviceLocked(exited)
	}
	p.start = nil
	p.mu.Unlock()

	start.err = err
	close(start.done)

	if err != nil {
		killChild(cmd, dead)
		return
	}
	go p.reap(exited)
}

func (p *serverProcess) waitReady(exited chan struct{}) error {
	ctx := context.Background()
	deadline := time.Now().Add(p.startupTimeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		select {
		case <-exited:
			return fmt.Errorf("%s: server exited during startup", p.name)
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/health", nil)
		if err != nil {
			return fmt.Errorf("%s: build health request: %w", p.name, err)
		}
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s: server did not become ready at %s within %v", p.name, p.baseURL, p.startupTimeout)
		}
		time.Sleep(p.pollInterval)
	}
}

func (p *serverProcess) reap(exited chan struct{}) {
	interval := p.idle / 4
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()

	for {
		select {
		case <-exited:
			return
		case <-tick.C:
		}

		p.mu.Lock()
		if p.closed || p.exited != exited || !p.aliveLocked() {
			p.mu.Unlock()
			return
		}
		if (p.shouldKeepAlive != nil && p.shouldKeepAlive()) || time.Since(p.lastUse) < p.idle {
			p.mu.Unlock()
			continue
		}
		slog.Info(p.logTag+" idle, shutting it down", "idle_for", time.Since(p.lastUse).Round(time.Second))
		cmd, dead := p.detachLocked()
		p.mu.Unlock()
		killChild(cmd, dead)
		return
	}
}

func (p *serverProcess) detachLocked() (*exec.Cmd, chan struct{}) {
	if !p.aliveLocked() {
		p.cmd = nil
		return nil, nil
	}
	cmd, exited := p.cmd, p.exited
	p.cmd = nil
	return cmd, exited
}

func (p *serverProcess) pid() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.aliveLocked() {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *serverProcess) killChildForTest() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.aliveLocked() {
		p.cmd.Process.Kill()
	}
}

func (p *serverProcess) Close() error {
	p.mu.Lock()
	p.closed = true
	cmd, dead := p.detachLocked()
	p.mu.Unlock()
	killChild(cmd, dead)
	return nil
}

// killChild SIGTERMs a detached child and waits for it to go, escalating to SIGKILL after five seconds. Called outside p.mu — nothing else may block on a dying process. A nil cmd is a no-op.
func killChild(cmd *exec.Cmd, exited chan struct{}) {
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

// dropDeviceLocked takes the GPU device flag out of the server's arguments after it died during startup, so the next start lets llama-server pick whatever it can find, down to the CPU. A named device can vanish under a running system: on 2026-09-23 a driver upgrade left the RTX 3050 out of Vulkan until a reboot, and the server refused "--device Vulkan1" on every start, so every embedding failed. Input: the exit channel of the start that failed, closed when the process died rather than timed out. Output: none; must be called with p.mu held.
func (p *serverProcess) dropDeviceLocked(exited chan struct{}) {
	select {
	case <-exited:
	default:
		return // still alive but slow: not a refused device
	}
	for i, a := range p.args {
		if (a == "--device" || a == "-dev") && i+1 < len(p.args) {
			slog.Warn(p.logTag+" would not start on its GPU device, starting it on whatever device it finds", "device", p.args[i+1])
			p.args = append(append([]string{}, p.args[:i]...), p.args[i+2:]...)
			return
		}
	}
}
