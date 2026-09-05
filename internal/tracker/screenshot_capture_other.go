//go:build !linux

package tracker

import (
	"context"
	"errors"
)

// CaptureFront has no screenshot path off Linux, so a look says so rather than returning an empty picture.
func CaptureFront(ctx context.Context) (Capture, error) {
	return Capture{}, errors.New("looking at the screen is not supported on this platform")
}
