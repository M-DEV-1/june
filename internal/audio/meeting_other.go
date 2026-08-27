//go:build !linux

package audio

import (
	"errors"
	"io"
	"time"
)

// MeetingCapture exists so callers compile on non-Linux; only the Linux build can actually record a meeting.
type MeetingCapture struct {
	MicStart    time.Time
	SystemStart time.Time
}

func StartMeetingCapture(mic, system io.Writer) (*MeetingCapture, error) {
	return nil, errors.New("meeting recording is implemented for PulseAudio on Linux only")
}

func (c *MeetingCapture) Stop() {}
