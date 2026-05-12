package tracker_test

import (
	"ora/internal/tracker"
	"testing"
)

// poll the os for active window
// normalize the retrieved content
// ignore noise

func TestTracker_GetActiveWindow(t *testing.T) {
	eye, err := tracker.New()
	if err != nil {
		t.Fatalf("Failed to initialize tracker: %v", err)
	}

	activity, err := eye.GetActiveWindow() // TODO: improve the capturing mechanism
	if err != nil {
		t.Fatalf("Failed to capture active window: %v", err)
	}

	if activity.App == "" || activity.Title == "" {
		t.Error("Expected app and title to not be empty")
	}

	t.Logf("Successfully capture window. App: %s | Title: %s", activity.App, activity.Title)
}
