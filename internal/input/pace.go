package input

import "time"

// keyDelay is the pause after every synthesized key event; the shell drops keys sent faster than a person could type. Matches internal/tracker/act_linux.go's keyDelay.
const keyDelay = 12 * time.Millisecond
