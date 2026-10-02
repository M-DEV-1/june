package tracker

// SetCapturer replaces the screen capture function. Used in tests to avoid real capture.
func (d *Daemon) SetCapturer(fn func() string) {
	d.capturer = fn
}
