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
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"june/internal/util"
)

// uiaScript is the UI Automation host script. It reaches PowerShell over stdin, never as a file.
//
//go:embed uia.ps1
var uiaScript string

// uiaRequest is one request line to the host script. Every field is always sent, so the script never has to ask whether a key is present.
type uiaRequest struct {
	Op   string `json:"op"`   // walk, desc, act, scroll, focused or focus
	Hwnd int64  `json:"hwnd"` // the window a walk or focus read is for; a desc, act, scroll or focused request carries the window its ref was read from as well, only so that uiaCall can refuse a hung one, and the script reads it for walk and focus alone
	Ref  string `json:"ref"`  // the element a desc, act, scroll or focused request is for
	Text bool   `json:"text"` // whether a walk is a capture walk, which reads TextPattern text; for a focus read, whether the element's contents are read whole (see FocusedContents)
	MS   int    `json:"ms"`   // how long a walk may take before it answers with what it has, or an act may take to find its element and wait for its action to return; uiaCall cuts it to what the deadline leaves
}

// uiaReplyMargin is how much of a request's deadline is kept back from the time the host is told it may spend, for it to stop, serialize its answer and for this side to read it. A walk of a 560-element Electron window told 1500 ms answered after 1582 ms on this desktop, serialization included.
const uiaReplyMargin = 500 * time.Millisecond

// uiaMinBudget is the least time worth sending a walk or an act with. Less than this after the margin means the deadline was spent waiting for a turn or for a cold host to start, and the request is not sent at all.
const uiaMinBudget = 200 * time.Millisecond

// uiaSent marks an error that came after a request reached the host: it was killed for not answering in time, it died, or it answered something unreadable. Whatever the request asked for may already have happened, which is what DoAction needs to know before anything presses the element a second time.
type uiaSent struct{ error }

func (e uiaSent) Unwrap() error { return e.error }

// uiaHost is one running copy of the host script: its process, the pipe requests go into, and the lines it answers with, which is closed when its output ends.
type uiaHost struct {
	proc    *os.Process
	stdin   io.WriteCloser
	lines   chan []byte
	ready   bool      // whether the script's ready line has been read
	started time.Time // when the process was started, to give up on one that never gets ready
}

// uiaStartLimit is how long a host may take to print its ready line before it is killed and the next call starts a fresh one. PowerShell plus its first Add-Type takes a few seconds; a script that threw before its ready line, such as under Constrained Language Mode or an AppLocker rule that blocks Add-Type's compiler, never prints it.
const uiaStartLimit = 30 * time.Second

var (
	// uiaTurn lets one request at a time talk to the host; holding it is what guards uiaLive.
	uiaTurn = make(chan struct{}, 1)
	// uiaLive is the running host, nil until the first request or after one was killed or died.
	uiaLive *uiaHost
)

// uiaCall sends one request to the host script and waits for its answer, starting the host first when none is running. Input: a context bounding the whole call, waiting for a turn included, and the request. Output: the reply, or an error naming what failed, the script's own error message included.
// A context that ends while the host is still starting leaves it starting, since PowerShell plus its first Add-Type takes a few seconds and killing it there would mean it never finishes; a context that ends while a request is in flight kills the host, because a UI Automation call into a hung application does not come back, and the next call starts a fresh one.
func uiaCall(ctx context.Context, req uiaRequest) (uiaReply, error) {
	// A UI Automation call into a window that stopped pumping messages does not come back, which would cost a host restart and its Add-Type compile on every read.
	if req.Hwnd != 0 && winHung(windows.HWND(req.Hwnd)) {
		return uiaReply{}, errors.New("the window is not responding")
	}
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
	if !h.ready && time.Since(h.started) > uiaStartLimit {
		h.kill()
		return uiaReply{}, fmt.Errorf("the UI Automation host was not ready after %s, so it was stopped", uiaStartLimit)
	}
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

	// The time the host may spend comes out of what is left of the caller's deadline, never more than the caller asked for. Waiting for a turn and a cold host's start (0.5 to 2 s) come out of that same deadline, so a fixed budget sent after a slow start overran it, the host was killed for that, and the next call started cold and overran again.
	if req.MS > 0 {
		if dl, ok := ctx.Deadline(); ok {
			left := time.Until(dl) - uiaReplyMargin
			if left < uiaMinBudget {
				// Nothing has been sent, so the host stays, ready for the next call.
				return uiaReply{}, fmt.Errorf("no time was left to ask UI Automation once its host was ready: %w", context.DeadlineExceeded)
			}
			req.MS = min(req.MS, int(left/time.Millisecond))
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
			return uiaReply{}, uiaSent{errors.New("the UI Automation host exited during a request")}
		}
		var r uiaReply
		if err := json.Unmarshal(uiaJSON(line), &r); err != nil {
			// A line that is not a reply means the script and this side no longer agree on which answer is whose.
			h.kill()
			return uiaReply{}, uiaSent{fmt.Errorf("the UI Automation host answered something unreadable: %w", err)}
		}
		if r.Err != "" {
			return r, errors.New(r.Err)
		}
		return r, nil
	case <-ctx.Done():
		h.kill()
		return uiaReply{}, uiaSent{fmt.Errorf("UI Automation did not answer in time, so its host was restarted: %w", ctx.Err())}
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
	cmd.Stderr = uiaStderr{}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	// Windows has no parent-death signal, and a host blocked in a UI Automation call into a hung application never reads the end of its stdin, so without the daemon's kill-on-close job it outlived a crashed daemon.
	util.KillWithDaemon(cmd)
	h := &uiaHost{proc: cmd.Process, stdin: stdin, lines: make(chan []byte), started: time.Now()}
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

// uiaStderr logs whatever the host writes to stderr, which is where PowerShell reports a script error such as one that stops the host before its ready line.
type uiaStderr struct{}

func (uiaStderr) Write(p []byte) (int, error) {
	slog.Warn("the UI Automation host reported an error", "stderr", strings.TrimSpace(string(p)))
	return len(p), nil
}

// uiaJSON finds the JSON object in one line of the host's output. Input: the line. Output: the line from its first "{" on, or nil for a line with none, such as anything PowerShell itself prints, which may also sit in front of a reply on the same line.
func uiaJSON(line []byte) []byte {
	if i := bytes.IndexByte(line, '{'); i >= 0 {
		return line[i:]
	}
	return nil
}

// winRef is one top-level window: its handle, the file name of the executable of the application it shows (such as "chrome.exe"; for a store app the one hosted in its frame, see winPid), its title and that executable's process id.
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
	procIsHung      = uiaUser32.NewProc("IsHungAppWindow")
	procIsIconic    = uiaUser32.NewProc("IsIconic")
	procFindChild   = uiaUser32.NewProc("FindWindowExW")
	procGetWindow   = uiaUser32.NewProc("GetWindow")
	gwlStyle        = int32(-16)
	gwlExStyle      = int32(-20)
	gwOwner         = uintptr(4)
	wsCaption       = uint32(0x00C00000)
	wsExTopmost     = uint32(0x8)
	wsExToolWindow  = uint32(0x80)
	wsExNoActivate  = uint32(0x08000000)
	winEnumMu       sync.Mutex
	winEnumOut      []windows.HWND
	winEnumCallback = windows.NewCallback(func(h windows.HWND, _ uintptr) uintptr {
		winEnumOut = append(winEnumOut, h)
		return 1
	})
	// coreWindowClass is the class of the window a store app draws in, a child of its ApplicationFrameWindow frame.
	coreWindowClass, _ = windows.UTF16PtrFromString("Windows.UI.Core.CoreWindow")
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

// winClass reads a window's class name. Output: the name, "" when the window has gone.
func winClass(h windows.HWND) string {
	buf := make([]uint16, 256)
	n, err := windows.GetClassName(h, &buf[0], int32(len(buf)))
	if err != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}

// winPid reads the id of the process whose application a window shows. Input: the window handle. Output: the pid, 0 when the window has gone, and whether it is a store app's, found inside the window's frame.
// Every store app's top-level window is an ApplicationFrameWindow belonging to ApplicationFrameHost.exe, so the frame's own process names Calculator, Settings and the rest alike; the app runs in the process of the CoreWindow hosted inside the frame, and that is the pid returned. A suspended app's CoreWindow leaves its frame, and the frame's own pid is all there is then.
func winPid(h windows.HWND) (uint32, bool) {
	var pid uint32
	windows.GetWindowThreadProcessId(h, &pid) //nolint:errcheck
	if winClass(h) != "ApplicationFrameWindow" {
		return pid, false
	}
	for c := winCoreChild(h, 0); c != 0; c = winCoreChild(h, c) {
		var app uint32
		windows.GetWindowThreadProcessId(c, &app) //nolint:errcheck
		if app != 0 && app != pid {
			return app, true
		}
	}
	return pid, false
}

// winCoreChild finds the next CoreWindow directly inside a frame. Input: the frame and the child to search after, 0 to start. Output: the child, 0 when there are no more.
func winCoreChild(frame, after windows.HWND) windows.HWND {
	c, _, _ := procFindChild.Call(uintptr(frame), uintptr(after), uintptr(unsafe.Pointer(coreWindowClass)), 0)
	return windows.HWND(c)
}

// exeOf reads the file name of a process's executable, such as "chrome.exe". Input: the pid. Output: the name, "" when the process cannot be opened, such as an elevated one.
func exeOf(pid uint32) string {
	p, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(p) //nolint:errcheck
	buf := make([]uint16, 1024)
	size := uint32(len(buf))
	if windows.QueryFullProcessImageName(p, 0, &buf[0], &size) != nil {
		return ""
	}
	return filepath.Base(windows.UTF16ToString(buf[:size]))
}

// winOf reads whose a window is and what it is called. Input: the window handle. Output: its winRef; exe is "" when the process cannot be opened, such as an elevated one.
func winOf(h windows.HWND) winRef {
	pid, framed := winPid(h)
	w := winRef{hwnd: h, title: winTitle(h), pid: pid}
	w.exe = exeOf(w.pid)
	if framed && w.exe != "" {
		storeApps.Store(strings.ToLower(trimExe(w.exe)), struct{}{})
	}
	return w
}

// winListable reports whether a window is one a person would call open: visible, titled, not cloaked (a window on another virtual desktop, or a suspended store app) and not a tool window.
func winListable(h windows.HWND) bool { return winOpen(h, false) }

// winListableAnyDesktop is winListable with windows on the other virtual desktops let in. The meeting reads use it: a call left running on another desktop is still the call.
func winListableAnyDesktop(h windows.HWND) bool { return winOpen(h, true) }

// winOpen is winListable and winListableAnyDesktop. Input: the window, and whether a window on another virtual desktop counts. Output: whether it is open.
func winOpen(h windows.HWND, otherDesktops bool) bool {
	if h == 0 || !windows.IsWindowVisible(h) || winTitle(h) == "" || winExStyle(h)&wsExToolWindow != 0 {
		return false
	}
	var cloaked uint32
	if windows.DwmGetWindowAttribute(h, windows.DWMWA_CLOAKED, unsafe.Pointer(&cloaked), 4) == nil && cloaked != 0 {
		return otherDesktops && cloaked == dwmCloakedShell && winOnOtherDesktop(h)
	}
	return true
}

// winExStyle reads a window's extended style bits.
func winExStyle(h windows.HWND) uint32 {
	ex, _, _ := procWinLong.Call(uintptr(h), uintptr(gwlExStyle))
	return uint32(ex)
}

// winPopup reports whether a window is a transient surface floating over an application rather than a window the user works in: one that never takes activation (a WinUI "Pop-upHost" holding a KeyTip or a tooltip, a flyout), or an owned, captionless window smaller than minLookSide (a menu, a dropdown). Input: the window. Output: true for such a popup.
// An owned window always sits directly above its owner in the z-order, so the scan in frontWindow met the popup first and read or pictured a 170x73 KeyTip host in place of the window under it. A dialog the user works in has a caption or is a window's size, so it is still picked.
func winPopup(h windows.HWND) bool {
	if winExStyle(h)&wsExNoActivate != 0 || popupClasses[winClass(h)] {
		return true
	}
	if owner, _, _ := procGetWindow.Call(uintptr(h), gwOwner); owner == 0 {
		return false
	}
	if style, _, _ := procWinLong.Call(uintptr(h), uintptr(gwlStyle)); uint32(style)&wsCaption == wsCaption {
		return false
	}
	frame, ok := windowFrame(h)
	return ok && (frame.Dx() < minLookSide || frame.Dy() < minLookSide)
}

// popupClasses are the window classes XAML hosts a windowed popup in: PopupWindowSiteBridge for WinUI 3 (titled "Pop-upHost", Win11 Notepad's KeyTips among them) and Xaml_WindowedPopupClass for UWP and the shell's own XAML (titled "PopupHost"). The one measured here carries WS_EX_NOACTIVATE as well; the class is checked too so that a popup whose style says otherwise is still never taken for the window under it.
// Win11 Notepad 11.2607 names the WinUI 3 one in full, Microsoft.UI.Content.PopupWindowSiteBridge, so both spellings are kept.
var popupClasses = map[string]bool{"PopupWindowSiteBridge": true, "Microsoft.UI.Content.PopupWindowSiteBridge": true, "Xaml_WindowedPopupClass": true}

// winIconic reports whether a window is minimised.
func winIconic(h windows.HWND) bool {
	r, _, _ := procIsIconic.Call(uintptr(h))
	return r != 0
}

// dwmCloakedShell is DWM_CLOAKED_SHELL, the DWMWA_CLOAKED value of a window the shell hid: one on another virtual desktop, and also a suspended store app's frame.
const dwmCloakedShell = 2

var (
	ole32            = windows.NewLazySystemDLL("ole32.dll")
	procCoCreateInst = ole32.NewProc("CoCreateInstance")
	// CLSID_VirtualDesktopManager and IID_IVirtualDesktopManager, from shobjidl_core.h.
	clsidVirtualDesktopManager = windows.GUID{Data1: 0xAA509086, Data2: 0x5CA9, Data3: 0x4C25, Data4: [8]byte{0x8F, 0x95, 0x58, 0x9D, 0x3C, 0x07, 0xB4, 0x8A}}
	iidVirtualDesktopManager   = windows.GUID{Data1: 0xA5CD92FF, Data2: 0x29BE, Data3: 0x454C, Data4: [8]byte{0x8D, 0x04, 0xD8, 0x28, 0x79, 0xFB, 0x3F, 0x1B}}
)

// virtualDesktopManager is an IVirtualDesktopManager: IUnknown's three methods, then IsWindowOnCurrentVirtualDesktop, GetWindowDesktopId and MoveWindowToDesktop.
type virtualDesktopManager struct{ vtbl *[6]uintptr }

// winOnOtherDesktop reports whether a shell-cloaked window is cloaked because it sits on another virtual desktop rather than because it is hidden. Input: the window. Output: true only when IVirtualDesktopManager says it is not on the current desktop and names the desktop it is on.
// Both halves are needed, measured on Windows 11 build 26200: a window moved to another desktop read DWMWA_CLOAKED 2, not on the current desktop, on that desktop's id; a suspended store app's frame (Settings) reads the same cloak but on the current desktop and on no desktop id, as does a hidden window.
func winOnOtherDesktop(h windows.HWND) bool {
	// COM is set up per thread, so the goroutine stays on this one until the object is released.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	switch err := windows.CoInitializeEx(0, windows.COINIT_MULTITHREADED); err {
	case nil, windows.Errno(windows.S_FALSE): // S_FALSE is a thread that already had COM, and it too is balanced by CoUninitialize
		defer windows.CoUninitialize()
	case windows.Errno(windows.RPC_E_CHANGED_MODE): // the thread already has COM as a single-threaded apartment, which serves as well
	default:
		return false
	}
	var m *virtualDesktopManager
	if r, _, _ := procCoCreateInst.Call(uintptr(unsafe.Pointer(&clsidVirtualDesktopManager)), 0, windows.CLSCTX_INPROC_SERVER|windows.CLSCTX_LOCAL_SERVER, uintptr(unsafe.Pointer(&iidVirtualDesktopManager)), uintptr(unsafe.Pointer(&m))); r != 0 || m == nil {
		return false
	}
	defer syscall.SyscallN(m.vtbl[2], uintptr(unsafe.Pointer(m))) //nolint:errcheck // Release
	var current int32
	if r, _, _ := syscall.SyscallN(m.vtbl[3], uintptr(unsafe.Pointer(m)), uintptr(h), uintptr(unsafe.Pointer(&current))); r != 0 || current != 0 {
		return false
	}
	var desk windows.GUID
	r, _, _ := syscall.SyscallN(m.vtbl[4], uintptr(unsafe.Pointer(m)), uintptr(h), uintptr(unsafe.Pointer(&desk)))
	return r == 0 && desk != windows.GUID{}
}

// shellClasses are the window classes of the shell's own surfaces that belong to explorer.exe: the desktop, the taskbars, the tray's overflow, and the Alt-Tab and Task View switchers (MultitaskingViewFrame before Windows 11, XamlExplorerHostIslandWindow since). File Explorer is explorer.exe too, as CabinetWClass, so these are told apart by class.
var shellClasses = map[string]bool{
	"Progman": true, "WorkerW": true,
	"Shell_TrayWnd": true, "Shell_SecondaryTrayWnd": true,
	"NotifyIconOverflowWindow": true, "TopLevelWindowForOverflowXamlIsland": true,
	"MultitaskingViewFrame": true, "XamlExplorerHostIslandWindow": true, "ForegroundStaging": true,
}

// winShell reports whether a window is a surface of the Windows shell rather than an application: the desktop, a taskbar, Start, search, the notification and quick-settings panes, the task switcher, the lock screen. They take the foreground like any window and none of them is something the user is doing. Input: the window, its application's name without ".exe" and its title. Output: true for a shell surface.
func winShell(h windows.HWND, app, title string) bool {
	if _, shell := nonWindowApps[strings.ToLower(app)]; shell && app != "" {
		return true
	}
	if shellClasses[winClass(h)] {
		return true
	}
	// The taskbar's buttons and the switchers' transient windows are explorer's with no title at all; a File Explorer window always has one.
	return strings.EqualFold(app, "explorer") && strings.TrimSpace(title) == ""
}

// winHung reports whether Windows considers a window hung: it has not answered messages for several seconds.
func winHung(h windows.HWND) bool {
	r, _, _ := procIsHung.Call(uintptr(h))
	return r != 0
}

// trimExe drops a trailing ".exe" in any case. Input: an executable's file name. Output: the name without it, such as "chrome".
func trimExe(s string) string {
	if strings.EqualFold(filepath.Ext(s), ".exe") {
		return s[:len(s)-4]
	}
	return s
}

// frontWindow picks the window the user is working in: the foreground window, or, when that is June's own or not a listable window, the one the user was in before it. Output: the window and true, false when there is none.
// Windows raises a window to the top of its band of the z-order when it is activated, so the one used last before June's is the first ordinary window below it. Topmost windows sit above that band however long ago they were used — a picture-in-picture video, a meeting's mini window, an overlay — so one is taken only when no ordinary window is open; a minimised window keeps its place in the z-order but is not on the screen to be looked at; and a popup (see winPopup) sits above the window it belongs to without ever having been the one in use.
func frontWindow() (winRef, bool) {
	pick := func(h windows.HWND) (winRef, bool) {
		if !winListable(h) || winIconic(h) {
			return winRef{}, false
		}
		w := winOf(h)
		return w, !IsJuneWindow(trimExe(w.exe), w.title)
	}
	fg := windows.GetForegroundWindow()
	if w, ok := pick(fg); ok {
		return w, true
	}
	var topmost winRef
	found := false
	for _, h := range topWindows() {
		if h == fg {
			continue
		}
		w, ok := pick(h)
		if !ok || winPopup(h) {
			continue
		}
		if winExStyle(h)&wsExTopmost == 0 {
			return w, true
		}
		if !found {
			topmost, found = w, true
		}
	}
	return topmost, found
}
