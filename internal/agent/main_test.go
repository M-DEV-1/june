package agent

import (
	"context"
	"os"
	"testing"

	"june/internal/act"
)

// TestMain gives the package a placeholder Gemini key. The router only offers Gemini when a key is set, and the ask tests answer through a fake Gemini server at geminiBaseURL; without a key they were routed past it to whatever real Claude or Codex login the machine running them has, which spent that plan and failed the test.
// It also has nothing hold the keyboard unless a test says so (see holdsKeyboard). type_text, press_key and a field_holds check all ask who holds the keyboard now, and the real read is of the window in front on the machine running the tests, so a password box or a Send button the developer happened to have focused decided what a test saw.
func TestMain(m *testing.M) {
	if os.Getenv("GEMINI_API_KEY") == "" {
		os.Setenv("GEMINI_API_KEY", "test-placeholder")
	}
	keyboardHolder = func(context.Context) (act.Node, bool) { return act.Node{}, false }
	keyboardContents = func(context.Context) (act.Node, bool) { return act.Node{}, false }
	os.Exit(m.Run())
}
