//go:build !windows

package util

import (
	"errors"
	"os/exec"
)

// ErrBatchArgument is never returned outside Windows; it exists so a caller can test for it on every platform.
var ErrBatchArgument = errors.New("not started")

// CheckCommandLine has nothing to check outside Windows: argv reaches the program as separate strings, never re-parsed by a shell.
func CheckCommandLine(cmd *exec.Cmd) error { return nil }

// ShortPath returns path unchanged: outside Windows a program gets its arguments as the bytes sent.
func ShortPath(path string) string { return path }

// ANSIText returns s unchanged, for the same reason.
func ANSIText(s string) string { return s }
