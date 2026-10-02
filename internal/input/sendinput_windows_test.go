package input

import "testing"

// A press and release of Shift on the real desktop, which changes nothing on screen: it fails if SendInput refuses the INPUT layout, which is the one thing about this file a Linux machine cannot check. Run it from an ordinary, unlocked session.
func TestPressShift(t *testing.T) {
	if err := new(Session).PressKey("shift"); err != nil {
		t.Fatal(err)
	}
}
