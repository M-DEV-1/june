//go:build taskeval && linux

package taskeval

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"june/internal/db"
	"june/taskeval/fixture"
)

// daemon is one running june --daemon with its own data directory, port and token.
type daemon struct {
	cmd   *exec.Cmd
	base  string
	token string
	dir   string
}

// startDaemon writes world into a fresh temp data directory, starts june --daemon on a free port with no desktop session in its environment, and waits until it answers. The daemon is killed when the test ends, and the tail of its log is printed if the test failed.
func startDaemon(t *testing.T, world fixture.World) *daemon {
	t.Helper()
	if live && os.Getenv("GEMINI_API_KEY") == "" {
		t.Skip("TASKEVAL_LIVE=1 needs GEMINI_API_KEY in the environment")
	}
	root := t.TempDir()
	data, home := filepath.Join(root, "data"), filepath.Join(root, "home")
	for _, dir := range []string{data, home} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// Setup not done keeps screen observation paused, and no window keeps the daemon from looking for one.
	if err := os.WriteFile(filepath.Join(data, "june-config.json"), []byte(`{"setup_done": false, "window": false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := db.New(filepath.Join(data, "db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := world.Write(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	store.Close()

	port := freePort(t)
	env := []string{
		// A bare PATH keeps the daemon from finding and running the developer's own agy, claude or codex.
		"PATH=/usr/bin:/bin",
		"HOME=" + home,
		"XDG_DATA_HOME=" + filepath.Join(home, "share"),
		"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
		"XDG_CACHE_HOME=" + filepath.Join(home, "cache"),
		"JUNE_DATA_DIR=" + data,
		"JUNE_PORT=" + port,
		"JUNE_NO_TRAY=1",
	}
	if live {
		env = append(env, "GEMINI_API_KEY="+os.Getenv("GEMINI_API_KEY"))
	} else {
		env = append(env, "GEMINI_API_KEY=taskeval-dummy-key", "GOOGLE_GEMINI_BASE_URL="+model.url())
	}

	out, err := os.Create(filepath.Join(root, "daemon.out"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(juneBin, "--daemon")
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = root, env, out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	d := &daemon{cmd: cmd, base: "http://127.0.0.1:" + port, dir: data}
	t.Cleanup(func() {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		cmd.Wait()
		out.Close()
		if t.Failed() {
			t.Logf("daemon log tail:\n%s", tail(filepath.Join(data, "june.log"), 15))
		}
	})
	d.waitReady(t)
	return d
}

// freePort returns a TCP port on 127.0.0.1 that nothing is listening on.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

// waitReady polls /ping until the daemon answers, then reads its token and checks that an authenticated route accepts it.
func (d *daemon) waitReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(d.base + "/ping"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(filepath.Join(d.dir, "ipc-token")); err == nil && len(raw) > 0 {
			d.token = strings.TrimSpace(string(raw))
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if d.token == "" {
		t.Fatalf("daemon did not become ready within 30s")
	}
	req, _ := http.NewRequest(http.MethodGet, d.base+"/status", nil)
	req.Header.Set("X-June-Token", d.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("daemon /status: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("daemon /status answered %s", resp.Status)
	}
}

// tail returns the last n lines of the file at path, or the read error.
func tail(path string, n int) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// event is one /events line as far as the harness reads it.
type event struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Text   string `json:"text"`
	Detail string `json:"detail"`
}

// answer is what /ask came back with for one question.
type answer struct {
	Text  string
	Steps int
}

// ask sends question to the daemon's gemini brain and returns its final answer once the done event arrives. Steps is the number of tool calls the daemon reported for the ask. The error says so if the stream ends, the ask fails, or ctx runs out first.
func (d *daemon) ask(ctx context.Context, question string) (answer, error) {
	streamCtx, stop := context.WithCancel(ctx)
	defer stop()
	events, err := d.openEvents(streamCtx)
	if err != nil {
		return answer{}, err
	}

	body, _ := json.Marshal(map[string]any{"question": question, "brain": "gemini"})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, d.base+"/ask", bytes.NewReader(body))
	req.Header.Set("X-June-Token", d.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return answer{}, err
	}
	defer resp.Body.Close()
	var accepted struct{ ID string }
	if resp.StatusCode != http.StatusAccepted || json.NewDecoder(resp.Body).Decode(&accepted) != nil {
		return answer{}, fmt.Errorf("POST /ask answered %s", resp.Status)
	}

	var got answer
	for {
		select {
		case ev := <-events:
			if ev.ID != accepted.ID {
				continue
			}
			switch ev.Type {
			case "tool":
				got.Steps++
			case "answer":
				got.Text = ev.Text
			case "error":
				return got, fmt.Errorf("ask failed: %s", ev.Text)
			case "done":
				got.Steps /= 2 // each tool call is reported twice, before it runs and after
				return got, nil
			}
		case <-ctx.Done():
			return got, fmt.Errorf("no answer before the deadline: %w", ctx.Err())
		}
	}
}

// openEvents opens GET /events and returns a channel of the events it carries. It returns once the daemon has accepted the subscription, so events from an ask posted afterwards are not missed. The channel stops filling when ctx ends.
func (d *daemon) openEvents(ctx context.Context) (<-chan event, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, d.base+"/events", nil)
	req.Header.Set("X-June-Token", d.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET /events answered %s", resp.Status)
	}
	out := make(chan event, 256)
	go func() {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			data, ok := strings.CutPrefix(sc.Text(), "data: ")
			if !ok {
				continue
			}
			var ev event
			if json.Unmarshal([]byte(data), &ev) == nil {
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// peakRSSKB is the sum of VmHWM, the peak resident memory, over the daemon and every process descended from it that is still alive. Processes that have already exited are not counted.
func (d *daemon) peakRSSKB() (int64, error) {
	var total int64
	for _, pid := range descendants(d.cmd.Process.Pid) {
		kb, err := peakKB(pid)
		if err != nil {
			continue // exited between the scan and the read
		}
		total += kb
	}
	if total == 0 {
		return 0, fmt.Errorf("could not read VmHWM of pid %d", d.cmd.Process.Pid)
	}
	return total, nil
}

// descendants returns pid and every process whose parent chain leads to it, read from /proc.
func descendants(pid int) []int {
	parent := map[int]int{}
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		p, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", p))
		if err != nil {
			continue
		}
		// The command name is in parentheses and may hold spaces, so fields are counted from the last ")": state, then ppid.
		rest := string(raw[bytes.LastIndexByte(raw, ')')+1:])
		if f := strings.Fields(rest); len(f) > 1 {
			parent[p], _ = strconv.Atoi(f[1])
		}
	}
	found, queue := []int{pid}, []int{pid}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for p, pp := range parent {
			if pp == cur {
				found, queue = append(found, p), append(queue, p)
			}
		}
	}
	return found
}

// peakKB reads the VmHWM line of /proc/<pid>/status, in kB.
func peakKB(pid int) (int64, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if rest, ok := strings.CutPrefix(line, "VmHWM:"); ok {
			return strconv.ParseInt(strings.Fields(rest)[0], 10, 64)
		}
	}
	return 0, fmt.Errorf("no VmHWM in /proc/%d/status", pid)
}
