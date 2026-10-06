//go:build windows

package cmd

import "golang.org/x/sys/windows"

// stderrUnseen reports whether nothing will show what this process writes to stderr. junew.exe is built without a console, so started from the Start menu or the sign-in entry it has no standard handles at all, and a program that starts it without handing any gets the NUL device; a console has a reader, and so does a pipe or a file a script set up.
func stderrUnseen() bool {
	h, err := windows.GetStdHandle(windows.STD_ERROR_HANDLE)
	if err != nil || h == 0 || h == windows.InvalidHandle {
		return true
	}
	kind, err := windows.GetFileType(h)
	if err != nil {
		return true
	}
	if kind != windows.FILE_TYPE_CHAR {
		return false
	}
	// NUL is a character device too; only a console answers GetConsoleMode.
	var mode uint32
	return windows.GetConsoleMode(h, &mode) != nil
}

// showFailure puts msg in a message box, for a junew.exe whose failure would otherwise reach only june.log. It waits for the box to be dismissed, which is all this process has left to do.
func showFailure(msg string) {
	text, err := windows.UTF16PtrFromString(msg)
	if err != nil {
		return
	}
	title, _ := windows.UTF16PtrFromString("June")
	windows.MessageBox(0, text, title, windows.MB_OK|windows.MB_ICONWARNING|windows.MB_SETFOREGROUND)
}
