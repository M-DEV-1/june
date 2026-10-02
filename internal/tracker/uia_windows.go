//go:build windows

package tracker

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// uiaScript is the UI Automation host script. It reaches PowerShell over stdin, never as a file.
//
//go:embed uia.ps1
var uiaScript string

// uiaRequest is one request line to the host script. Every field is always sent, so the script never has to ask whether a key is present.
type uiaRequest struct {
	Op   string `json:"op"`   // walk, desc, act, scroll, focused or focus
	Hwnd int64  `json:"hwnd"` // the window a walk or focus read is for
	Ref  string `json:"ref"`  // the element a desc, act, scroll or focused request is for
	Text bool   `json:"text"` // whether a walk is a capture walk, which reads TextPattern text
	MS   int    `json:"ms"`   // how long a walk may take before it answers with what it has
}

// uiaHost is one running copy of the host script: its process, the pipe requests go into, and the lines it answers with, which is closed when its output ends.
type uiaHost struct {
	proc  *os.Process
	stdin io.WriteCloser
	lines chan []byte
	ready bool // whether the script's ready line has been read
}

var (
	// uiaTurn lets one request at a time talk to the host; holding it is what guards uiaLive.
	uiaTurn = make(chan struct{}, 1)
	// uiaLive is the running host, nil until the first request or after one was killed or died.
	uiaLive *uiaHost
)

// uiaCall sends one request to the host script and waits for its answer, starting the host first when none is running. Input: a context bounding the whole call, waiting for a turn included, and the request. Output: the reply, or an error naming what failed, the script's own error message included.
// A context that ends while the host is still starting leaves it starting, since PowerShell plus its first Add-Type takes a few seconds and killing it there would mean it never finishes; a context that ends while a request is in flight kills the host, because a UI Automation call into a hung application does not come back, and the next call starts a fresh one.
func uiaCall(ctx context.Context, req uiaRequest) (uiaReply, error) {
	select {
	case uiaTurn <- struct{}{}:
	case <-ctx.Done():
		return uiaReply{}, fmt.Errorf("UI Automation is busy with another read: %w", ctx.Err())
	}
	defer func() { <-uiaTurn }()

	if uiaLive == nil {
		h, err := startUIAHost()
		if err != nil {
			return uiaReply{}, fmt.Errorf("cannot start the UI Automation host: %w", err)
		}
		uiaLive = h
	}
	h := uiaLive
	for !h.ready {
		select {
		case line, ok := <-h.lines:
			if !ok {
				uiaLive = nil
				return uiaReply{}, errors.New("the UI Automation host exited while starting")
			}
			h.ready = bytes.Contains(uiaJSON(line), []byte(`"ready"`))
		case <-ctx.Done():
			return uiaReply{}, fmt.Errorf("the UI Automation host is still starting: %w", ctx.Err())
		}
	}

	b, err := json.Marshal(req)
	if err != nil {
		return uiaReply{}, err
	}
	if _, err := io.WriteString(h.stdin, "UiaServe '"+base64.StdEncoding.EncodeToString(b)+"'\n"); err != nil {
		h.kill()
		return uiaReply{}, fmt.Errorf("the UI Automation host stopped taking requests: %w", err)
	}
	select {
	case line, ok := <-h.lines:
		if !ok {
			uiaLive = nil
			return uiaReply{}, errors.New("the UI Automation host exited during a request")
		}
		var r uiaReply
		if err := json.Unmarshal(uiaJSON(line), &r); err != nil {
			// A line that is not a reply means the script and this side no longer agree on which answer is whose.
			h.kill()
			return uiaReply{}, fmt.Errorf("the UI Automation host answered something unreadable: %w", err)
		}
		if r.Err != "" {
			return r, errors.New(r.Err)
		}
		return r, nil
	case <-ctx.Done():
		h.kill()
		return uiaReply{}, fmt.Errorf("UI Automation did not answer in time, so its host was restarted: %w", ctx.Err())
	}
}

// kill stops a host and forgets it, and drains its output so the goroutine reading it can finish. Called with uiaTurn held.
func (h *uiaHost) kill() {
	h.proc.Kill() //nolint:errcheck // a host that already exited is the outcome wanted
	go func() {
		for range h.lines {
		}
	}()
	if uiaLive == h {
		uiaLive = nil
	}
}

// powershellPath is Windows PowerShell's full path, so a powershell.exe earlier on PATH is never the one run. Output: the path under %SystemRoot%, or the bare name when SystemRoot is unset.
func powershellPath() string {
	if root := os.Getenv("SystemRoot"); root != "" {
		return filepath.Join(root, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	}
	return "powershell.exe"
}

// startUIAHost starts PowerShell with no window, reading statements from stdin, and hands it the script as its first statement. Output: the host, not yet ready, or an error when PowerShell cannot be started.
// -Command - is used rather than passing the script on the command line because powershell.exe given a command and a redirected stdin reads that stdin as the command's input and may not run the command until it closes.
func startUIAHost() (*uiaHost, error) {
	cmd := exec.Command(powershellPath(), "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", "-")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	h := &uiaHost{proc: cmd.Process, stdin: stdin, lines: make(chan []byte)}
	// The script line is some kilobytes, more than a pipe holds before PowerShell starts reading, so it is written on its own goroutine and the caller's context governs the wait for the ready line instead.
	go func() {
		line := "iex ([Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('" + base64.StdEncoding.EncodeToString([]byte(uiaScript)) + "')))\n"
		if _, err := io.WriteString(stdin, line); err != nil {
			cmd.Process.Kill() //nolint:errcheck
		}
	}()
	go func() {
		br := bufio.NewReader(stdout)
		for {
			line, err := br.ReadBytes('\n')
			if uiaJSON(line) != nil {
				h.lines <- line
			}
			if err != nil {
				break
			}
		}
		close(h.lines)
		cmd.Wait() //nolint:errcheck // the exit status says nothing a caller acts on
	}()
	return h, nil
}

// uiaJSON finds the JSON object in one line of the host's output. Input: the line. Output: the line from its first "{" on, or nil for a line with none, such as anything PowerShell itself prints, which may also sit in front of a reply on the same line.
func uiaJSON(line []byte) []byte {
	if i := bytes.IndexByte(line, '{'); i >= 0 {
		return line[i:]
	}
	return nil
}

// winRef is one top-level window: its handle, the file name of the executable that owns it (such as "chrome.exe"), its title and its process id.
type winRef struct {
	hwnd  windows.HWND
	exe   string
	title string
	pid   uint32
}

var (
	uiaUser32       = windows.NewLazySystemDLL("user32.dll")
	procWinText     = uiaUser32.NewProc("GetWindowTextW")
	procWinLong     = uiaUser32.NewProc("GetWindowLongW")
	gwlExStyle      = int32(-20)
	wsExToolWindow  = uint32(0x80)
	winEnumMu       sync.Mutex
	winEnumOut      []windows.HWND
	winEnumCallback = windows.NewCallback(func(h windows.HWND, _ uintptr) uintptr {
		winEnumOut = append(winEnumOut, h)
		return 1
	})
)

// topWindows lists every top-level window, front to back in z-order. The EnumWindows callback is made once for the process, because Windows allows only a limited number of them.
func topWindows() []windows.HWND {
	winEnumMu.Lock()
	defer winEnumMu.Unlock()
	winEnumOut = nil
	windows.EnumWindows(winEnumCallback, nil) //nolint:errcheck // a partial list is still the best answer there is
	return winEnumOut
}

// winTitle reads a window's title. Output: the trimmed title, "" when it has none.
func winTitle(h windows.HWND) string {
	buf := make([]uint16, 512)
	n, _, _ := procWinText.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return strings.TrimSpace(windows.UTF16ToString(buf[:n]))
}

// winOf reads who owns a window and what it is called. Input: the window handle. Output: its winRef; exe is "" when the process cannot be opened, such as an elevated one.
func winOf(h windows.HWND) winRef {
	w := winRef{hwnd: h, title: winTitle(h)}
	windows.GetWindowThreadProcessId(h, &w.pid) //nolint:errcheck
	p, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, w.pid)
	if err != nil {
		return w
	}
	defer windows.CloseHandle(p) //nolint:errcheck
	buf := make([]uint16, 1024)
	size := uint32(len(buf))
	if windows.QueryFullProcessImageName(p, 0, &buf[0], &size) == nil {
		w.exe = filepath.Base(windows.UTF16ToString(buf[:size]))
	}
	return w
}

// winListable reports whether a window is one a person would call open: visible, titled, not cloaked (a window on another virtual desktop, or a suspended store app) and not a tool window.
func winListable(h windows.HWND) bool {
	if h == 0 || !windows.IsWindowVisible(h) || winTitle(h) == "" {
		return false
	}
	var cloaked uint32
	if windows.DwmGetWindowAttribute(h, windows.DWMWA_CLOAKED, unsafe.Pointer(&cloaked), 4) == nil && cloaked != 0 {
		return false
	}
	ex, _, _ := procWinLong.Call(uintptr(h), uintptr(gwlExStyle))
	return uint32(ex)&wsExToolWindow == 0
}

// trimExe drops a trailing ".exe" in any case. Input: an executable's file name. Output: the name without it, such as "chrome".
func trimExe(s string) string {
	if strings.EqualFold(filepath.Ext(s), ".exe") {
		return s[:len(s)-4]
	}
	return s
}

// frontWindow picks the window the user is working in: the foreground window, or, when that is June's own or not a listable window, the first listable window behind it that is not June's. Output: the window and true, false when there is none.
func frontWindow() (winRef, bool) {
	pick := func(h windows.HWND) (winRef, bool) {
		if !winListable(h) {
			return winRef{}, false
		}
		w := winOf(h)
		return w, !IsJuneWindow(trimExe(w.exe), w.title)
	}
	if w, ok := pick(windows.GetForegroundWindow()); ok {
		return w, true
	}
	for _, h := range topWindows() {
		if w, ok := pick(h); ok {
			return w, true
		}
	}
	return winRef{}, false
}
