//go:build !windows && !linux

package audio

import "fmt"

func NewMic() (Microphone, error) {
	return nil, fmt.Errorf("audio not supported on this platform")
}

func NewSpeaker() (Speaker, error) {
	return nil, fmt.Errorf("audio not supported on this platform")
}
