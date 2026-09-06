package cmd

import (
	"testing"
	"time"
)

// Shutdown runs its steps one after another, and a step that never finishes holds the process open for good: the port stays bound and the next daemon cannot start. Each step gets a bound, and within is what makes it real, so a step still running when its bound passes is left behind rather than waited on.
func TestWithin_ReturnsWhenAStepNeverFinishes(t *testing.T) {
	forever := make(chan struct{})
	defer close(forever)

	start := time.Now()
	within("a step that hangs", 20*time.Millisecond, func() { <-forever })
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Fatalf("shutdown waited %v on a 20ms bound", elapsed)
	}
}

// A step that finishes must not be waited on for its whole bound: shutdown should cost what the steps cost, not the sum of their ceilings.
func TestWithin_ReturnsAsSoonAsTheStepIsDone(t *testing.T) {
	ran := false
	start := time.Now()
	within("a step that finishes", 10*time.Second, func() { ran = true })
	elapsed := time.Since(start)

	if !ran {
		t.Fatal("the step never ran")
	}
	if elapsed > time.Second {
		t.Fatalf("a step that returned at once still cost %v", elapsed)
	}
}
