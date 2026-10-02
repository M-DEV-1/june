//go:build windows

package cmd

import (
	"golang.org/x/sys/windows/registry"
)

// audioChecks reports whether call detection can see which programs are using the microphone. Windows records through WASAPI, which every desk has, so there is no audio server to check. Input: ignored, it is $XDG_RUNTIME_DIR on Linux. Output: one check.
// Call detection reads the per-user microphone consent store (internal/recorder/meetingwatch_windows.go), so the check opens the same key.
func audioChecks(string) []doctorCheck {
	const key = `Software\Microsoft\Windows\CurrentVersion\CapabilityAccessManager\ConsentStore\microphone`
	k, err := registry.OpenKey(registry.CURRENT_USER, key, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return []doctorCheck{{Name: "call detection", Detail: `cannot open HKCU\` + key + ": " + err.Error() + "; noticing a call and offering to record it is off", Fix: "turn on Settings > Privacy & security > Microphone, and let desktop apps access the microphone"}}
	}
	k.Close()
	return []doctorCheck{{Name: "call detection", Detail: `microphone consent store readable at HKCU\` + key, OK: true}}
}
