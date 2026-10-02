//go:build !linux && !windows

package tracker

import "context"

// WarmUpScreenshotPermission is a no-op on platforms with no screenshot path: only the xdg-desktop-portal path on Linux needs an up-front consent prompt.
func WarmUpScreenshotPermission(ctx context.Context) {}
