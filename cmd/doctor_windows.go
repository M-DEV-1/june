//go:build windows

package cmd

import (
	"golang.org/x/sys/windows/registry"
)

// platformChecks reports what the desktop itself must provide: the WebView2 runtime the window renders in, and the microphone consent store call detection reads. Windows records through WASAPI, which every desk has, so there is no audio server to check. Input: ignored, it is $XDG_RUNTIME_DIR on Linux. Output: one check each.
func platformChecks(string) []doctorCheck {
	return []doctorCheck{webView2Check(), callDetectionCheck()}
}

// webView2ClientKey is the EdgeUpdate client key of the WebView2 Evergreen runtime; an installed runtime leaves its version in the key's pv value.
const webView2ClientKey = `Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}`

// webView2Check reports whether the WebView2 runtime is installed, per machine or per user, which is where Microsoft's own installers record it. Without it the window cannot start at all.
func webView2Check() doctorCheck {
	for _, loc := range []struct {
		root registry.Key
		name string
		path string
	}{
		{registry.LOCAL_MACHINE, "HKLM", `SOFTWARE\WOW6432Node\` + webView2ClientKey},
		{registry.LOCAL_MACHINE, "HKLM", `SOFTWARE\` + webView2ClientKey},
		{registry.CURRENT_USER, "HKCU", `Software\` + webView2ClientKey},
	} {
		k, err := registry.OpenKey(loc.root, loc.path, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		pv, _, err := k.GetStringValue("pv")
		k.Close()
		if err == nil && pv != "" && pv != "0.0.0.0" {
			return doctorCheck{Name: "window runtime", Detail: "WebView2 " + pv + " at " + loc.name + `\` + loc.path, OK: true}
		}
	}
	return doctorCheck{Name: "window runtime", Detail: "no WebView2 runtime found under HKLM or HKCU; the June window cannot open without it", Fix: "install Microsoft's WebView2 Evergreen Bootstrapper from https://developer.microsoft.com/microsoft-edge/webview2/"}
}

// callDetectionCheck reports whether call detection can see which programs are using the microphone. It reads the per-user microphone consent store (internal/recorder/meetingwatch_windows.go), so the check opens the same key.
func callDetectionCheck() doctorCheck {
	const key = `Software\Microsoft\Windows\CurrentVersion\CapabilityAccessManager\ConsentStore\microphone`
	k, err := registry.OpenKey(registry.CURRENT_USER, key, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return doctorCheck{Name: "call detection", Detail: `cannot open HKCU\` + key + ": " + err.Error() + "; noticing a call and offering to record it is off", Fix: "turn on Settings > Privacy & security > Microphone, and let desktop apps access the microphone"}
	}
	k.Close()
	return doctorCheck{Name: "call detection", Detail: `microphone consent store readable at HKCU\` + key, OK: true}
}
