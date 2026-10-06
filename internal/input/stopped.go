package input

import "fmt"

// Stopped is what TypeTextContext and PressKeyContext return when their context ends part way through: how many of the characters, or of a chord's keys, had gone out before it did, and what ended it. Every key that went down has been released by the time it is returned, so nothing is left held for whatever is sent next.
type Stopped struct {
	Sent, Of int
	Err      error
}

func (s *Stopped) Error() string {
	return fmt.Sprintf("input: stopped after %d of %d keys: %v", s.Sent, s.Of, s.Err)
}

func (s *Stopped) Unwrap() error { return s.Err }
