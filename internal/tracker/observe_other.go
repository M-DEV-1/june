//go:build !linux && !windows

package tracker

import (
	"context"
	"errors"

	"june/internal/act"
)

// Observe is Linux and Windows only; elsewhere it reports that the screen cannot be read. Input: a context. Output: empty values and an error.
func Observe(ctx context.Context) (app, title string, nodes []act.Node, err error) {
	return "", "", nil, errors.New("reading the screen is only supported on Linux and Windows")
}

func UseFocusReader(f func(ctx context.Context) (pid uint32, title string, ok bool)) {}
