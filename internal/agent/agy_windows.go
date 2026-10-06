package agent

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// fsctlSetReparsePoint is FSCTL_SET_REPARSE_POINT, which x/sys/windows does not export.
const fsctlSetReparsePoint = 0x000900A4

// agyCopyLimit is the largest file linkEntry copies into a throwaway HOME. The files it copies are agy's and the Gemini CLI's small JSON state at the top of ~/.gemini; anything bigger is not something a run needs a private snapshot of, and copying it on every session start would cost more than leaving it out.
const agyCopyLimit = 4 << 20

// linkEntry makes dst stand for src in the throwaway HOME. A symbolic link needs Developer Mode or an elevated token on Windows, which most desks have neither of, so when that is refused a directory becomes a junction, which needs no privilege and is the same directory under a second name: a SQLite database opened through it keeps its -wal and -shm beside the real file, so agy's own state is shared exactly as a symlink would share it.
// A file is copied instead. A hard link looked like the same thing for files but is not: SQLite names its -wal and -shm after the path it opened, so a database hard-linked into the mirror got a second WAL and SHM pair that the real one never saw, and a file agy rewrites by replacing it stops being shared the first time it is written (2026-10-03). A copy is honestly a private snapshot: what the run writes to it is the run's own and is thrown away with the HOME, while agy's real state lives in the directories, which are junctioned whole.
// A link this process cannot follow is worse than none, because agy inherits this process's mitigations and cannot follow it either: with Windows' RedirectionGuard on, every agy run died in a quarter of a second on "mkdir ...\.gemini\antigravity-cli: Cannot create a file when that file already exists" and "open ...\antigravity-cli\installation_id: The path cannot be traversed because it contains an untrusted mount point" (2026-10-06, on the June that Setup opened right after installing; see agyLinksFollowable). Such a link is taken out again and the entry becomes the run's own: an empty directory, which agy fills with fresh state (its login is in the keyring, not here), or a copy of a file.
func linkEntry(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		// An entry that cannot be read, such as a link in ~/.gemini whose target is gone, is left out: agy makes what it needs, and failing here would fail every run.
		slog.Debug("agy: an entry in ~/.gemini cannot be read, leaving it out of the run's HOME", "entry", src, "error", err)
		return nil
	}
	if agyLinksFollowable() {
		err := os.Symlink(src, dst)
		if err != nil && !errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
			return err
		}
		if err != nil && info.IsDir() {
			err = createJunction(src, dst)
			if err != nil {
				return err
			}
		}
		if err == nil {
			_, statErr := os.Stat(dst)
			if statErr == nil {
				return nil
			}
			// Only a refusal Windows makes for every link says the next link will fail too; anything else, such as the target going away since it was listed, is this entry's alone.
			if errors.Is(statErr, errorUntrustedMountPoint) {
				noteAgyLinksUnfollowable(statErr.Error())
			}
			// os.Remove takes away the link itself and never what it points at: a junction or a directory symlink goes with RemoveDirectory, a file symlink with DeleteFile.
			if err := os.Remove(dst); err != nil {
				return fmt.Errorf("removing a link agy could not follow: %w", err)
			}
		}
	}
	if info.IsDir() {
		return os.Mkdir(dst, 0o700)
	}
	// A copy of a login is a second credential on disk, in %TEMP%, that outlives any run whose HOME is not removed and is not touched by uninstalling June. agy does not read these: it signs in through the Windows credential store (its own log says "authenticated via keyring" on every run, June's throwaway HOMEs included), so leaving them out costs a run nothing.
	if agyCredentialFile(filepath.Base(src)) {
		return nil
	}
	if info.Size() > agyCopyLimit {
		slog.Debug("agy: a file in ~/.gemini is too big to copy into the run's HOME, leaving it out", "file", src, "bytes", info.Size())
		return nil
	}
	return copyFile(src, dst)
}

const (
	// processRedirectionTrustPolicy is ProcessRedirectionTrustPolicy in Windows' PROCESS_MITIGATION_POLICY enum, which x/sys/windows does not export; bit 0 of the policy's flags is EnforceRedirectionTrust.
	processRedirectionTrustPolicy = 16
	// errorUntrustedMountPoint is ERROR_UNTRUSTED_MOUNT_POINT, "The path cannot be traversed because it contains an untrusted mount point", which x/sys/windows does not export either.
	errorUntrustedMountPoint = syscall.Errno(448)
)

// agyLinks is what this process has learned about following the links it makes: unfollowable is set once one could not be followed, after which no more are made, and noted makes the reason logged once rather than once per entry and run.
var agyLinks struct {
	unfollowable atomic.Bool
	noted        sync.Once
}

// redirectionGuardOn reports whether Windows' RedirectionGuard is enforced on this process. With it, Windows refuses to follow any junction or symbolic link an unprivileged user made, and child processes inherit it (checked on 2026-10-06: a child of a process that turned it on reports it on too). Inno Setup turns it on for Setup.exe by default since 6.7, so the June that Setup opens at the end of an install or an update, and every agy that June starts, has it until June is started some other way. Output: false when it is off or the policy cannot be read.
func redirectionGuardOn() bool {
	var flags uint32
	r, _, _ := procGetProcessMitigationPolicy.Call(uintptr(windows.CurrentProcess()), processRedirectionTrustPolicy, uintptr(unsafe.Pointer(&flags)), unsafe.Sizeof(flags))
	return r != 0 && flags&1 != 0
}

var procGetProcessMitigationPolicy = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetProcessMitigationPolicy")

// agyLinksFollowable reports whether a link made into a throwaway HOME is worth making: not once one has failed to resolve, and not under RedirectionGuard, which this process passes on to agy.
func agyLinksFollowable() bool {
	if agyLinks.unfollowable.Load() {
		return false
	}
	if redirectionGuardOn() {
		noteAgyLinksUnfollowable("Windows RedirectionGuard is on for June, which it inherits when its installer opens it")
		return false
	}
	return true
}

// noteAgyLinksUnfollowable records that links into a throwaway HOME cannot be followed from this process, and logs why once. Input: the reason.
func noteAgyLinksUnfollowable(reason string) {
	agyLinks.unfollowable.Store(true)
	agyLinks.noted.Do(func() {
		slog.Warn("agy: this June cannot follow links into agy's own folders (as happens whenever its installer or an update opened it), so each run gets fresh folders of its own for as long as this June runs", "reason", reason)
	})
}

// createJunction makes dst a directory junction to target. The reparse buffer is a mount point's: the NT form of the target as the substitute name and the plain path as the print name, each NUL-terminated.
func createJunction(target, dst string) error {
	target, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	substitute, err := windows.UTF16FromString(`\??\` + target)
	if err != nil {
		return err
	}
	printName, err := windows.UTF16FromString(target)
	if err != nil {
		return err
	}
	subBytes := (len(substitute) - 1) * 2
	printBytes := (len(printName) - 1) * 2
	pathBuffer := append(substitute, printName...)
	// ReparseTag, ReparseDataLength, Reserved, then four offsets/lengths before the path buffer.
	const header = 8
	const mountHeader = 8
	dataLen := mountHeader + len(pathBuffer)*2
	buf := make([]byte, header+dataLen)
	le := func(off int, v uint16) { buf[off], buf[off+1] = byte(v), byte(v>>8) }
	tag := uint32(windows.IO_REPARSE_TAG_MOUNT_POINT)
	buf[0], buf[1], buf[2], buf[3] = byte(tag), byte(tag>>8), byte(tag>>16), byte(tag>>24)
	le(4, uint16(dataLen))
	le(8, 0)
	le(10, uint16(subBytes))
	le(12, uint16(subBytes+2))
	le(14, uint16(printBytes))
	for i, c := range pathBuffer {
		le(header+mountHeader+i*2, c)
	}

	if err := os.Mkdir(dst, 0o700); err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(dst)
	if err != nil {
		os.Remove(dst)
		return err
	}
	h, err := windows.CreateFile(name, windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		os.Remove(dst)
		return fmt.Errorf("opening %s for a junction: %w", dst, err)
	}
	var returned uint32
	err = windows.DeviceIoControl(h, fsctlSetReparsePoint, (*byte)(unsafe.Pointer(&buf[0])), uint32(len(buf)), nil, 0, &returned, nil)
	windows.CloseHandle(h)
	if err != nil {
		os.Remove(dst)
		return fmt.Errorf("making %s a junction to %s: %w", dst, target, err)
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
