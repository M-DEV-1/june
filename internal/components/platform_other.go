//go:build !windows && !linux

package components

import (
	"errors"
	"syscall"
)

// No component has a build for any other OS, so there is nothing to choose between and nothing to make room for.

func detectGPU() (GPU, bool) { return GPU{}, false }

func vulkanPresent() bool { return false }

func freeBytes(string) int64 { return 0 }

func totalRAMMB() int { return 0 }

func smartAppControl() string { return "off" }

func diskFull(err error) bool { return errors.Is(err, syscall.ENOSPC) }
