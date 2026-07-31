//go:build !linux

package tracker

import "context"

// WarmUpScreenshotPermission is a no-op off Linux: only the xdg-desktop-portal screenshot path needs an up-front consent prompt. Windows captures via UIA.
func WarmUpScreenshotPermission(ctx context.Context) {}
