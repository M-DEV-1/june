package util

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

const (
	// maxCommandLine is the longest command line CreateProcess takes, in UTF-16 units less the terminating NUL. A longer one fails with "The filename or extension is too long".
	maxCommandLine = 32766
	// maxBatchLine is the longest line cmd.exe runs, and a .cmd or .bat target is run as a cmd.exe line.
	maxBatchLine = 8191
	// cpUTF8 is the code page number of UTF-8, which x/sys/windows does not export.
	cpUTF8 = 65001
)

// ErrBatchArgument is what CheckCommandLine wraps when a .cmd or .bat target would not get an argument through cmd.exe intact, so a caller can say what to install instead.
var ErrBatchArgument = errors.New("not started")

// CheckCommandLine reports, before cmd is started, why Windows would not hand cmd's arguments to the program unchanged: a command line past what CreateProcess or cmd.exe accept, or an argument cmd.exe would read as something else.
// The second only touches a .cmd or .bat target, such as the shim npm installs a CLI as. Go quotes arguments for programs that split their command line the way the C runtime does, and cmd.exe splits it differently first: it ignores the backslash Go puts before an embedded quote, so `5" wide & echo hi` ends the quoted argument at the quote and runs the rest as a second command; it expands %NAME% even inside quotes; it ends the command at the first line break; and in an argument with no space or tab, which Go leaves unquoted, & | < > ^ are its operators, as ( ) are inside the IF block older npm shims run %* in. The quote, the expansion, the line break and a bare & were each reproduced through an npm-style shim on 2026-10-03.
// Input: a cmd built with exec.Command. Output: nil when the command line arrives intact, otherwise an error that names neither argument nor prompt, since callers log it.
func CheckCommandLine(cmd *exec.Cmd) error {
	name := filepath.Base(cmd.Path)
	var line []string
	for _, arg := range cmd.Args {
		line = append(line, syscall.EscapeArg(arg))
	}
	n := len(utf16.Encode([]rune(strings.Join(line, " "))))
	ext := strings.ToLower(filepath.Ext(cmd.Path))
	if ext != ".cmd" && ext != ".bat" {
		if n > maxCommandLine {
			return fmt.Errorf("%s: the command line would be %d characters, past the %d Windows allows", name, n, maxCommandLine)
		}
		return nil
	}
	for _, arg := range cmd.Args[1:] {
		unquoted := arg != "" && !strings.ContainsAny(arg, " \t")
		if strings.ContainsAny(arg, "\"%\r\n") || (unquoted && strings.ContainsAny(arg, "&|<>^()")) {
			return fmt.Errorf("%s runs through cmd.exe, which would rewrite an argument holding a quote, a percent sign, a line break or a bare shell operator, so it was %w", name, ErrBatchArgument)
		}
	}
	if n > maxBatchLine {
		return fmt.Errorf("%s runs through cmd.exe, and the command line would be %d characters, past the %d it allows", name, n, maxBatchLine)
	}
	return nil
}

// ShortPath returns the 8.3 short form of an existing path that holds a character outside ASCII, for a program whose main() reads its arguments in the ANSI code page: a character that code page lacks reaches it as '?', and the file cannot be opened. The short form is ASCII, so it survives. Input: a path to an existing file. Output: the short form, or path unchanged when it is already ASCII, when it does not exist, or when the volume keeps no short names.
func ShortPath(path string) string {
	if isASCII(path) {
		return path
	}
	long, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return path
	}
	buf := make([]uint16, windows.MAX_PATH)
	for {
		n, err := windows.GetShortPathName(long, &buf[0], uint32(len(buf)))
		if err != nil || n == 0 {
			return path
		}
		if int(n) > len(buf) {
			buf = make([]uint16, n)
			continue
		}
		if short := windows.UTF16ToString(buf[:n]); isASCII(short) {
			return short
		}
		return path
	}
}

// ANSIText returns what of s a program reading its arguments in the ANSI code page and treating them as UTF-8 still reads as s. Where the ANSI code page is UTF-8 that is all of it; anywhere else every character outside ASCII arrives as a byte of the local code page, or as '?', which is not the text sent, so those characters are dropped. Input: any text. Output: s, or s without its non-ASCII characters.
func ANSIText(s string) string {
	if isASCII(s) || windows.GetACP() == cpUTF8 {
		return s
	}
	return strings.Map(func(r rune) rune {
		if r < 0x80 {
			return r
		}
		return -1
	}, s)
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}
