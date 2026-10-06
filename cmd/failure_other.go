//go:build !windows && !linux

package cmd

// stderrUnseen is false outside Windows and Linux, where June has nothing to show a failure with: the line is in june.log as well.
func stderrUnseen() bool { return false }

// showFailure has nothing to show here, where stderrUnseen never asks for it.
func showFailure(string) {}
