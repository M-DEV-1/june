package window

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// fakeExtension stands in for the ora@ora.local shell extension on a private test bus: it records each call it receives and answers with a canned bool, so the test can prove the Raiser sends the right method name and argument without a real gnome-shell.
type fakeExtension struct {
	mu    sync.Mutex
	calls []string
	want  string // the pid/title/wmclass value that should be reported found
}

func (f *fakeExtension) record(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
}

func (f *fakeExtension) List() (string, *dbus.Error) {
	f.record("List()")
	return "[]", nil
}

func (f *fakeExtension) ActivateByPid(pid uint32) (bool, *dbus.Error) {
	f.record(fmt.Sprintf("ActivateByPid(%d)", pid))
	return fmt.Sprint(pid) == f.want, nil
}

func (f *fakeExtension) ActivateByTitle(substring string) (bool, *dbus.Error) {
	f.record("ActivateByTitle(" + substring + ")")
	return substring == f.want, nil
}

func (f *fakeExtension) ActivateByWmClass(wmClass string) (bool, *dbus.Error) {
	f.record("ActivateByWmClass(" + wmClass + ")")
	return wmClass == f.want, nil
}

// startPrivateBus launches a throwaway dbus-daemon for the test to talk to, so the test never touches the real session bus. Input: none. Output: the bus's address and a func that shuts it down, or t.Skip if no dbus-daemon binary is on PATH.
func startPrivateBus(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("dbus-daemon"); err != nil {
		t.Skip("no dbus-daemon on PATH; skipping live D-Bus test")
	}
	cmd := exec.Command("dbus-daemon", "--session", "--print-address", "--nofork")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("pipe dbus-daemon stdout: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start dbus-daemon: %v", err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })

	addrCh := make(chan string, 1)
	go func() {
		s := bufio.NewScanner(stdout)
		if s.Scan() {
			addrCh <- s.Text()
		}
	}()
	select {
	case addr := <-addrCh:
		return addr
	case <-time.After(5 * time.Second):
		t.Fatal("dbus-daemon never printed an address")
		return ""
	}
}

// serveFakeExtension connects to the private bus as "org.gnome.Shell" and exports the fake extension at the real object path, exactly as the real gnome-shell would once ora@ora.local is enabled.
func serveFakeExtension(t *testing.T, addr string, ext *fakeExtension) {
	t.Helper()
	conn, err := dbus.Connect(addr)
	if err != nil {
		t.Fatalf("connect fake server to private bus: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	reply, err := conn.RequestName(busName, dbus.NameFlagDoNotQueue)
	if err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("claim %s on private bus: reply=%v err=%v", busName, reply, err)
	}

	methods := map[string]any{
		"List":              ext.List,
		"ActivateByPid":     ext.ActivateByPid,
		"ActivateByTitle":   ext.ActivateByTitle,
		"ActivateByWmClass": ext.ActivateByWmClass,
	}
	if err := conn.ExportMethodTable(methods, objectPath, ifaceName); err != nil {
		t.Fatalf("export fake extension: %v", err)
	}
}

func TestRaiser_CallsTheRightMethodWithTheRightArgument(t *testing.T) {
	addr := startPrivateBus(t)
	ext := &fakeExtension{want: "1234"}
	serveFakeExtension(t, addr, ext)

	t.Setenv("DBUS_SESSION_BUS_ADDRESS", addr)
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if ok, err := r.Available(ctx); err != nil || !ok {
		t.Fatalf("Available: ok=%v err=%v", ok, err)
	}
	if ok, err := r.ByPid(ctx, 1234); err != nil || !ok {
		t.Fatalf("ByPid(1234): ok=%v err=%v, want true", ok, err)
	}
	if ok, err := r.ByPid(ctx, 9999); err != nil || ok {
		t.Fatalf("ByPid(9999): ok=%v err=%v, want false", ok, err)
	}

	ext.mu.Lock()
	calls := append([]string(nil), ext.calls...)
	ext.mu.Unlock()
	want := []string{"List()", "ActivateByPid(1234)", "ActivateByPid(9999)"}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Errorf("call %d = %q, want %q", i, calls[i], want[i])
		}
	}
}

func TestRaiser_ByTitleAndByWmClass(t *testing.T) {
	addr := startPrivateBus(t)
	ext := &fakeExtension{want: "Brave"}
	serveFakeExtension(t, addr, ext)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", addr)
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if ok, err := r.ByTitle(ctx, "Brave"); err != nil || !ok {
		t.Fatalf("ByTitle: ok=%v err=%v, want true", ok, err)
	}
	if ok, err := r.ByWmClass(ctx, "Brave"); err != nil || !ok {
		t.Fatalf("ByWmClass: ok=%v err=%v, want true", ok, err)
	}
	if ok, err := r.ByTitle(ctx, "Slack"); err != nil || ok {
		t.Fatalf("ByTitle(Slack): ok=%v err=%v, want false", ok, err)
	}
}

// TestRaiser_NoExtensionMeansUnavailable proves the argument shapes work end to end even when nothing is exported at the extension's object path: every call must return (false, non-nil error) rather than panicking, matching how the real Raiser behaves before the user has installed or enabled ora@ora.local.
func TestRaiser_NoExtensionMeansUnavailable(t *testing.T) {
	addr := startPrivateBus(t)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", addr)
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if ok, err := r.Available(ctx); ok || err == nil {
		t.Fatalf("Available with no extension exported: ok=%v err=%v, want false and an error", ok, err)
	}
	if ok, err := r.ByPid(ctx, 1); ok || err == nil {
		t.Fatalf("ByPid with no extension exported: ok=%v err=%v, want false and an error", ok, err)
	}
}

// The daemon holds one Raiser for its whole life and closes it with everything else it opened, so Close has to actually release the bus connection rather than leave it to process exit.
func TestRaiser_CloseReleasesTheConnection(t *testing.T) {
	addr := startPrivateBus(t)
	serveFakeExtension(t, addr, &fakeExtension{})
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", addr)
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if ok, err := r.Available(ctx); err != nil || !ok {
		t.Fatalf("Available before Close: ok=%v err=%v", ok, err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if ok, err := r.Available(ctx); ok || err == nil {
		t.Errorf("Available after Close: ok=%v err=%v, want false and an error from a closed connection", ok, err)
	}
}
