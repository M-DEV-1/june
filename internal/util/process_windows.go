package util

import (
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	createNoWindow  = 0x08000000
	detachedProcess = 0x00000008
)

// ChildProcAttr starts a spawned server with no console window. Windows has no parent-death signal, so pair it with KillWithDaemon after Start.
func ChildProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: createNoWindow}
}

// OwnProcessGroup makes cmd start in a new process group with no console window. Start it with StartProcessGroup, which gives it the Job Object KillProcessGroup ends.
func OwnProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | createNoWindow}
}

var (
	runJobsMu sync.Mutex
	// runJobs is the Job Object each cmd StartProcessGroup started runs in, from just before its start until its release.
	runJobs = map[*exec.Cmd]windows.Handle{}
)

// StartProcessGroup starts cmd suspended, puts it in a Job Object of its own and only then lets it run, so every process it ever starts is created inside that job. Windows has no process group to kill by: taskkill /T follows live parent links, and a grandchild whose parent has already exited has none, so it was left running; a job holds every process created inside it whoever its parent was, and TerminateJobObject ends them all.
// The child joins the daemon's kill-on-close job first and its own job second, which nests its job inside the daemon's (a job can only be nested under one the process already belongs to, Windows 8 and later), so a crashed daemon still takes the whole tree with it.
// Input: a cmd set up with OwnProcessGroup, not yet started. Output: a func to call once cmd has been waited for, which lets go of the job without killing anything, and the start error. When no job can be had the child still runs, and KillProcessGroup falls back to taskkill.
func StartProcessGroup(cmd *exec.Cmd) (release func(), err error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		slog.Warn("could not make a job object for a child; killing it will fall back to taskkill", "error", err)
		return func() {}, cmd.Start()
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	// Registered before the start, because exec calls cmd.Cancel as soon as the context ends, which can be before the child has joined the job.
	runJobsMu.Lock()
	runJobs[cmd] = job
	runJobsMu.Unlock()
	release = func() {
		runJobsMu.Lock()
		defer runJobsMu.Unlock()
		if h, ok := runJobs[cmd]; ok {
			windows.CloseHandle(h)
			delete(runJobs, cmd)
		}
	}
	if err := cmd.Start(); err != nil {
		release()
		return func() {}, err
	}
	if err := joinJobs(job, cmd.Process.Pid); err != nil {
		slog.Warn("could not put a child in a job of its own; killing it will fall back to taskkill", "pid", cmd.Process.Pid, "error", err)
		release()
	}
	if err := resumeProcess(uint32(cmd.Process.Pid)); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		release()
		return func() {}, fmt.Errorf("could not resume %s after starting it suspended: %w", filepath.Base(cmd.Path), err)
	}
	return release, nil
}

// joinJobs puts the process pid into the daemon's kill-on-close job and then into job. Output: the error of joining job; failing to join the daemon's job is only logged, as KillWithDaemon does.
func joinJobs(job windows.Handle, pid int) error {
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	daemon, err := daemonJobHandle()
	if err == nil {
		err = windows.AssignProcessToJobObject(daemon, h)
	}
	if err != nil {
		slog.Warn("could not tie a child process to the daemon's lifetime; it may outlive a crash", "pid", pid, "error", err)
	}
	return windows.AssignProcessToJobObject(job, h)
}

// resumeProcess lets a process started with CREATE_SUSPENDED run. os/exec closes the thread handle CreateProcess returns, so the process's one thread is found again by its owner's pid.
func resumeProcess(pid uint32) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snap)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	resumed := false
	for next := windows.Thread32First(snap, &entry); next == nil; next = windows.Thread32Next(snap, &entry) {
		if entry.OwnerProcessID != pid {
			continue
		}
		t, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if err != nil {
			return err
		}
		_, err = windows.ResumeThread(t)
		windows.CloseHandle(t)
		if err != nil {
			return err
		}
		resumed = true
	}
	if !resumed {
		return fmt.Errorf("no thread of process %d to resume", pid)
	}
	return nil
}

// KillProcessGroup kills a started cmd and every process it started: the whole job when StartProcessGroup gave it one, and otherwise whatever taskkill /T can still reach through live parent links. Input: a started cmd. Output: the kill's error, if any.
func KillProcessGroup(cmd *exec.Cmd) error {
	runJobsMu.Lock()
	job, ok := runJobs[cmd]
	if ok {
		err := windows.TerminateJobObject(job, 1)
		runJobsMu.Unlock()
		// The child itself as well: a context that ends between the start and the child joining its job cancels a suspended child the job does not hold yet.
		cmd.Process.Kill()
		return err
	}
	runJobsMu.Unlock()
	kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	kill.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
	return kill.Run()
}

// Detach makes cmd start with no console and in a process group of its own, so it outlives the daemon.
func Detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess | syscall.CREATE_NEW_PROCESS_GROUP}
}

// HideConsole makes cmd start with no console window. The daemon has no console, so every console program it starts would otherwise open a window of its own.
func HideConsole(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
}

var (
	daemonJobOnce sync.Once
	daemonJob     windows.Handle
	daemonJobErr  error
)

// daemonJobHandle is the Job Object Windows closes when the daemon exits, made on first use.
func daemonJobHandle() (windows.Handle, error) {
	daemonJobOnce.Do(func() { daemonJob, daemonJobErr = newKillOnCloseJob() })
	return daemonJob, daemonJobErr
}

// KillWithDaemon puts a started child into a Job Object that Windows closes when the daemon exits, however it exits, and closing the job kills every process in it. This stands in for Linux's parent-death signal, so a crashed daemon does not leave llama-server holding the GPU. Input: a started cmd. Output: none; a failure is logged and the child runs on unguarded.
func KillWithDaemon(cmd *exec.Cmd) {
	job, err := daemonJobHandle()
	if err == nil {
		var h windows.Handle
		if h, err = windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid)); err == nil {
			err = windows.AssignProcessToJobObject(job, h)
			windows.CloseHandle(h)
		}
	}
	if err != nil {
		slog.Warn("could not tie a child process to the daemon's lifetime; it may outlive a crash", "pid", cmd.Process.Pid, "error", err)
	}
}

// newKillOnCloseJob creates the Job Object KillWithDaemon assigns children to. Its handle is never closed by hand: the process exiting closes it, which is what kills the children.
func newKillOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}
