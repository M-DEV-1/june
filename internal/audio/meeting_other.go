//go:build !linux && !windows

package audio

import (
	"errors"
	"io"
	"time"
)

// MeetingCapture exists so callers compile on platforms with no meeting capture; Linux and Windows have their own.
type MeetingCapture struct {
	MicStart    time.Time
	SystemStart time.Time
}

func StartMeetingCapture(mic, system io.Writer) (*MeetingCapture, error) {
	return nil, errors.New("meeting recording is implemented for Linux and Windows only")
}

func (c *MeetingCapture) Stop() {}
