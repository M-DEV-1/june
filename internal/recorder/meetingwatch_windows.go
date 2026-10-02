package recorder

import (
	"context"

	"golang.org/x/sys/windows/registry"
)

// micConsentKey is where Windows records, per user, which apps have used the microphone and whether they still are.
const micConsentKey = `Software\Microsoft\Windows\CurrentVersion\CapabilityAccessManager\ConsentStore\microphone`

// readMicUsers returns the apps holding the microphone now, read from the consent store: packaged apps are the subkeys of micConsentKey, desktop apps the subkeys of its NonPackaged key. It returns nothing when the key cannot be read, so the watcher simply never fires.
func readMicUsers(ctx context.Context) []string {
	entries := consentEntries(micConsentKey, true)
	entries = append(entries, consentEntries(micConsentKey+`\NonPackaged`, false)...)
	return consentUsers(entries)
}

// consentEntries reads the start and stop stamps of every app subkey under path in HKCU. A missing stamp reads as 0. Input: the key path and whether its subkeys are packaged apps. Output: one entry per app, or nil when the key cannot be opened.
func consentEntries(path string, packaged bool) []consentEntry {
	k, err := registry.OpenKey(registry.CURRENT_USER, path, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return nil
	}
	defer k.Close()
	names, err := k.ReadSubKeyNames(-1)
	if err != nil {
		return nil
	}
	var entries []consentEntry
	for _, n := range names {
		if packaged && n == "NonPackaged" {
			continue
		}
		sub, err := registry.OpenKey(k, n, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		start, _, _ := sub.GetIntegerValue("LastUsedTimeStart")
		stop, _, _ := sub.GetIntegerValue("LastUsedTimeStop")
		sub.Close()
		entries = append(entries, consentEntry{key: n, packaged: packaged, start: start, stop: stop})
	}
	return entries
}
