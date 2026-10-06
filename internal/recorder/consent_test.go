package recorder

import (
	"slices"
	"testing"
	"time"
)

// Windows' microphone consent store keeps a start and stop stamp per app; an app holds the microphone while it has started and not stopped. June's own process is left out, desktop apps are named from the exe at the end of the #-separated path, and packaged apps from the family name before the publisher hash, and an app listed twice is named once.
func TestConsentUsers(t *testing.T) {
	entries := []consentEntry{
		{key: `C:#Program Files#Zoom#bin#Zoom.exe`, start: 5},
		{key: `C:#Program Files (x86)#Microsoft#Edge#Application#msedge.exe`, start: 5, stop: 9},
		{key: `C:#Users#someone#AppData#Local#June#june.exe`, start: 5},
		{key: `C:#Users#someone#AppData#Local#June#junew.exe`, start: 5},
		{key: `C:#Program Files#Google#Chrome#Application#chrome.exe`, start: 5},
		{key: `C:#Other#chrome.exe`, start: 7},
		{key: `C:#Tools#never-used.exe`},
		{key: "MSTeams_8wekyb3d8bbwe", packaged: true, start: 3},
	}
	want := []string{"Zoom", "Chrome", "MSTeams"}
	var got []string
	for _, u := range consentUsers(entries, time.Now()) {
		got = append(got, u.name)
	}
	if !slices.Equal(got, want) {
		t.Errorf("consentUsers = %q, want %q", got, want)
	}
}
