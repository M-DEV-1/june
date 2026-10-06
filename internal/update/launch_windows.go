//go:build windows

package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"june/internal/util"
)

// canSelfInstall reports whether this copy of June was put there by the installer, which leaves its uninstaller beside the exes. The installer updates whatever directory it installed to before, so run from a portable or development copy it would install a second June elsewhere and point the sign-in entry at that one; those copies get the release page instead.
func canSelfInstall(exeDir string) bool {
	return exeDir != "" && util.Exists(filepath.Join(exeDir, "unins000.exe"))
}

const (
	// installerEnv and installLogEnv carry the two paths to cmd.exe. cmd expands %…% in its command line before it reads anything else, so a path written into the line itself would have any %NAME% in it (a legal folder name) replaced; a variable's value is not expanded a second time, and inside quotes nothing else a Windows path can hold is special to cmd.
	installerEnv  = "JUNE_UPDATE_INSTALLER"
	installLogEnv = "JUNE_UPDATE_LOG"
	// starterLine is cmd.exe's whole command line. /d skips any AutoRun command the user's registry adds to every cmd; start /b keeps a console installer, such as a stand-in for one, in cmd's hidden console rather than opening a window for it. /RELAUNCH is June's own switch: the installer only opens June again after a silent install when it is given (packaging/windows/june.iss).
	starterLine = `cmd.exe /d /v:off /c start "" /b "%` + installerEnv + `%" /VERYSILENT /SUPPRESSMSGBOXES /NORESTART /RELAUNCH "/LOG=%` + installLogEnv + `%"`
	// starterTimeout bounds cmd.exe, whose only work is starting the installer.
	starterTimeout = 30 * time.Second
)

// startInstaller runs the downloaded installer with no wizard and no prompts, and returns as soon as it is running. Input: the installer and the log file it should write. Output: a function that waits for the installer to exit and gives nil for exit code 0, or the error starting it.
// The installer is the one that stops this daemon, so it must not be anything that goes down with it. A child of June's is in June's process tree, and the installer's last resort for a daemon slow to quit, taskkill /T on June, ends the whole tree, installer included: nothing would be installed and nothing would reopen June. So cmd.exe starts it and exits at once, which leaves the installer's parent a process that is gone and no walk down June's tree reaching it. cmd itself leaves any job June runs in, as the installer would have to.
func startInstaller(path, logPath string) (func() error, error) {
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), starterTimeout)
	defer cancel()
	flags := uint32(windows.CREATE_BREAKAWAY_FROM_JOB | windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP)
	starter := starterCmd(ctx, filepath.Join(sys, "cmd.exe"), path, logPath, flags)
	err = starter.Start()
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		// A job that does not allow breakaway fails the whole CreateProcess with this. Started inside it, the installer still outlives the daemon unless that job is also killed when its creator exits, which is the best that can be had.
		starter = starterCmd(ctx, filepath.Join(sys, "cmd.exe"), path, logPath, flags&^windows.CREATE_BREAKAWAY_FROM_JOB)
		err = starter.Start()
	}
	if err != nil {
		return nil, err
	}
	if err := starter.Wait(); err != nil {
		return nil, fmt.Errorf("cmd.exe could not start the installer: %w", err)
	}
	h, err := openStarted(uint32(starter.Process.Pid), path)
	if err != nil {
		return nil, err
	}
	return func() error {
		defer windows.CloseHandle(h)
		if _, err := windows.WaitForSingleObject(h, windows.INFINITE); err != nil {
			return err
		}
		var code uint32
		if err := windows.GetExitCodeProcess(h, &code); err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("the installer exited with code %d", code)
		}
		return nil
	}, nil
}

// starterCmd builds one attempt at starting cmd.exe, run from the installer's folder rather than June's, so the folder being updated is not held open as the installer's working directory.
// It gets the environment June was started with rather than os.Environ(), which the installer inherits through cmd: the installer hands its environment to the June it reopens, which would otherwise find every key this daemon loaded from <data>/env already set, take them for the user's own environment variables, and never read the file's keys afresh again. Its standard handles are left unset, so they are NUL rather than pipes: an installer inheriting the write end of a pipe would hold Wait open until it exited.
func starterCmd(ctx context.Context, cmdExe, path, logPath string, flags uint32) *exec.Cmd {
	cmd := exec.CommandContext(ctx, cmdExe)
	cmd.Dir = filepath.Dir(path)
	cmd.Env = append(util.StartupEnviron(), installerEnv+"="+path, installLogEnv+"="+logPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: starterLine, CreationFlags: flags}
	return cmd
}

// openStarted finds the installer cmd.exe started, by its parent and by its program, and opens it to wait on. Input: cmd.exe's pid and the installer's path. Output: a handle the caller closes, or the error; an installer gone already has failed, since the one that succeeds stops this daemon first.
func openStarted(parent uint32, path string) (windows.Handle, error) {
	want, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(snap)
	e := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if e.ParentProcessID != parent || !strings.EqualFold(windows.UTF16ToString(e.ExeFile[:]), filepath.Base(path)) {
			continue
		}
		h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, e.ProcessID)
		if err != nil {
			return 0, err
		}
		// cmd.exe has exited, so its pid is free to be reused; the process is only taken for the installer when it runs the very file that was verified. The file is compared rather than the path, which can be spelt several ways.
		if got, err := imageOf(h); err == nil && os.SameFile(got, want) {
			return h, nil
		}
		windows.CloseHandle(h)
	}
	return 0, errors.New("the installer exited as soon as it started")
}

// imageOf reads the program file process h runs. Output: its file info, or the error.
func imageOf(h windows.Handle) (os.FileInfo, error) {
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return nil, err
	}
	return os.Stat(windows.UTF16ToString(buf[:n]))
}
