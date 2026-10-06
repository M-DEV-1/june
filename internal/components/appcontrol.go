package components

import (
	"errors"
	"runtime"
	"syscall"
)

// appControlError is a program Windows' application control would not let run: Smart App Control, or a policy whoever manages the PC set. Retrying cannot change it and neither can another build of the same program, which is just as unsigned, so it comes wrapped in startError, or as errCUDABlocked for ggml-cuda.dll, and finishFeature answers neither with the CPU build.
type appControlError struct{ msg string }

func (e appControlError) Error() string { return e.msg }

// appControlStart reads a program that would not start. CreateProcess fails with one of the Win32 errors below when a policy blocks the program itself; Explorer shows the same as the HRESULT 0x800711C7. Input: the program's file name and Start's error. Output: the error to report, and false when it was not a block.
func appControlStart(file string, err error) (error, bool) {
	var errno syscall.Errno
	if runtime.GOOS != "windows" || !errors.As(err, &errno) {
		return nil, false
	}
	n := uint32(errno)
	if n&0xFFFF0000 == 0x80070000 {
		n &= 0xFFFF
	}
	switch n {
	// ERROR_SYSTEM_INTEGRITY_POLICY_VIOLATION, then the reputation verdicts Smart App Control adds: malicious, potentially unwanted, dangerous extension, unfriendly, explicitly denied, not WHQL.
	case 4551, 4556, 4557, 4558, 4580, 4582, 4583:
		return appControlError{appControlMessage(file, false)}, true
	// No verdict, because Microsoft's reputation service could not be reached (4559) or failed (4581); the same program may well run once it can.
	case 4559, 4581:
		return appControlError{appControlMessage(file, true)}, true
	}
	return nil, false
}

// appControlExit reads a program that started and was stopped before its first line because a policy blocked a DLL it imports: the loader ends it with the NTSTATUS of the block, the same verdicts as appControlStart's. Input: the program's file name and its exit code. Output: the error to report, and false when the code is not a block.
func appControlExit(file string, code int) (error, bool) {
	if runtime.GOOS != "windows" {
		return nil, false
	}
	switch uint32(code) {
	case 0xC0E90002, 0xC0E90007, 0xC0E90008, 0xC0E90009, 0xC0E9000B, 0xC0E9000D, 0xC0E9000E:
		return appControlError{appControlMessage("a file "+file+" needs", false)}, true
	case 0xC0E9000A, 0xC0E9000C:
		return appControlError{appControlMessage("a file "+file+" needs", true)}, true
	}
	return nil, false
}

// appControlMessage is the sentence for a block, naming Smart App Control when it is the one on. It is shown on a card in the failed state, whose button reads Try again. Input: what was blocked, and whether the block was for want of an answer from Microsoft rather than a verdict.
func appControlMessage(what string, offline bool) string {
	sac := smartAppControl() == "on"
	switch {
	case offline && sac:
		return "Smart App Control could not check " + what + " with Microsoft, so it would not run; connect to the internet and choose Try again"
	case offline:
		return "Windows could not check " + what + " with Microsoft, so it would not run; connect to the internet and choose Try again"
	case sac:
		return "Smart App Control blocked " + what + ", and Windows cannot make an exception for one program while Smart App Control is on, so this feature cannot be used on this PC"
	}
	return "an app-control policy on this PC blocked " + what + ", so this feature cannot be used here; whoever manages this PC can allow it"
}
