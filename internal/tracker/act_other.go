//go:build !linux

package tracker

import (
	"context"
	"errors"
)

var errActUnsupported = errors.New("acting on the screen is only supported on Linux")

// DoAction is Linux-only; elsewhere it reports that the screen cannot be acted on.
func DoAction(ctx context.Context, ref string) (string, error) { return "", errActUnsupported }

// Verify is Linux-only; elsewhere it reports that the screen cannot be acted on.
func Verify(ctx context.Context, ref, role, label string, x, y, w, h int) error {
	return errActUnsupported
}

// Extents is Linux-only; elsewhere it reports that the screen cannot be read.
func Extents(ctx context.Context, ref string) (x, y, w, h int, err error) {
	return 0, 0, 0, 0, errActUnsupported
}

// ScrollTo is Linux-only; elsewhere it reports that the screen cannot be acted on.
func ScrollTo(ctx context.Context, ref string) error { return errActUnsupported }

// Focused is Linux-only; elsewhere it reports that the screen cannot be read.
func Focused(ctx context.Context, ref string) (bool, error) { return false, errActUnsupported }
