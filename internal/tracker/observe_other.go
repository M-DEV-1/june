//go:build !linux

package tracker

import (
	"context"
	"errors"

	"ora/internal/act"
)

// Observe is Linux-only; elsewhere it reports that the screen cannot be read. Input: a context. Output: empty values and an error.
func Observe(ctx context.Context) (app, title string, nodes []act.Node, err error) {
	return "", "", nil, errors.New("reading the screen is only supported on Linux")
}
