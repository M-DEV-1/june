//go:build !linux

package tracker

import "context"

// newOCR has no OCR program to call on this platform, so a window that publishes no text is recorded by its title alone.
func newOCR() func(ctx context.Context, png []byte) string { return nil }
