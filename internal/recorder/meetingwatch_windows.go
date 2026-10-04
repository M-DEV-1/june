package recorder

import (
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// micConsentKey is where Windows records, per user, which apps have used the microphone and whether they still are.
const micConsentKey = `Software\Microsoft\Windows\CurrentVersion\CapabilityAccessManager\ConsentStore\microphone`

// readMicUsers returns the apps holding the microphone now, read from the consent store: packaged apps are the subkeys of micConsentKey, desktop apps the subkeys of its NonPackaged key. It returns nothing when the key cannot be read, so the watcher simply never fires.
func readMicUsers(ctx context.Context) []micHolder {
	entries := consentEntries(micConsentKey, true)
	entries = append(entries, consentEntries(micConsentKey+`\NonPackaged`, false)...)
	return consentUsers(entries, time.Now())
}

// consentEntries reads the start and stop stamps of every app subkey under path in HKCU, and resolves the apps holding the microphone to their executable and display name. A missing stamp reads as 0. Input: the key path and whether its subkeys are packaged apps. Output: one entry per app, or nil when the key cannot be opened.
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
		e := consentEntry{key: n, packaged: packaged, start: start, stop: stop}
		// The store lists every app that ever asked, several hundred on a developer's machine, and only a holder is ever named, so only a holder costs a manifest or version-resource read.
		if start != 0 && stop == 0 {
			e.proc, e.name = resolveHolder(n, packaged)
		}
		entries = append(entries, e)
	}
	return entries
}

// resolvedApp is what resolveHolder found for one consent key; empty fields are what it could not find.
type resolvedApp struct{ proc, name string }

// resolvedApps caches what resolveHolder found, by consent key, for the daemon's lifetime. The watcher asks every five seconds for as long as a call lasts, and neither a package's executable and display name nor an exe's description changes under it.
var resolvedApps sync.Map

// resolveHolder names the app behind one consent key. Input: the key, a package family name or a #-separated exe path, and which of the two it is. Output: the executable its windows run under (only a package needs one; a desktop app's is the end of its key) and the name Windows shows for it, each "" when it could not be read.
func resolveHolder(key string, packaged bool) (proc, name string) {
	if v, ok := resolvedApps.Load(key); ok {
		r := v.(resolvedApp)
		return r.proc, r.name
	}
	if packaged {
		proc, name = packageApp(key)
	} else {
		name = exeDescription(strings.ReplaceAll(key, "#", `\`))
	}
	// A lookup that found nothing is not remembered, since it can fail for a reason that passes — mid-update, the family can list the outgoing version whose folder is being removed — and caching it would name the app by its package until the daemon restarts. Retrying costs a few calls every five seconds, and only while an app nothing could be read for holds the microphone.
	if proc != "" || name != "" {
		resolvedApps.Store(key, resolvedApp{proc: proc, name: name})
	}
	return proc, name
}

var (
	procGetPackagesByPackageFamily = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetPackagesByPackageFamily")
	procGetPackagePathByFullName   = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetPackagePathByFullName")
	procSHLoadIndirectString       = windows.NewLazySystemDLL("shlwapi.dll").NewProc("SHLoadIndirectString")
)

// appxManifest is the part of a package's AppxManifest.xml that names it: its identity, the DisplayName it is listed under, and the executable and tile name of each app it holds.
type appxManifest struct {
	Identity struct {
		Name string `xml:"Name,attr"`
	} `xml:"Identity"`
	DisplayName string `xml:"Properties>DisplayName"`
	Apps        []struct {
		Executable string `xml:"Executable,attr"`
		Visual     struct {
			DisplayName string `xml:"DisplayName,attr"`
		} `xml:"VisualElements"`
	} `xml:"Applications>Application"`
}

// packageApp resolves a packaged app's consent key to the executable its windows run under and the name it is shown by. Input: the package family name, such as MSTeams_8wekyb3d8bbwe. Output: the first app's executable without its .exe ("ms-teams"; "WhatsApp.Root" for WhatsApp) and the package's DisplayName ("Microsoft Teams"), each "" when it cannot be read, and consentUsers then falls back to the family name.
// Both come from the installed package's AppxManifest.xml, found through the documented package API rather than the AppModel repository keys in the registry, whose layout Windows does not promise. The tracker and WindowTitleFor know a window by its process's executable, and a package's family name is not that: MSTeams runs as ms-teams.exe and 5319275A.WhatsAppDesktop as WhatsApp.Root.exe, neither a substring of the other.
func packageApp(family string) (proc, name string) {
	full := packageFullName(family)
	if full == "" {
		return "", ""
	}
	dir := packagePath(full)
	if dir == "" {
		return "", ""
	}
	b, err := os.ReadFile(filepath.Join(dir, "AppxManifest.xml"))
	if err != nil {
		return "", ""
	}
	var m appxManifest
	if err := xml.Unmarshal(b, &m); err != nil {
		return "", ""
	}
	name = indirectString(full, m.Identity.Name, m.DisplayName)
	for _, a := range m.Apps {
		if a.Executable == "" {
			continue
		}
		proc = filepath.Base(a.Executable)
		if ext := filepath.Ext(proc); strings.EqualFold(ext, ".exe") {
			proc = strings.TrimSuffix(proc, ext)
		}
		if name == "" {
			name = indirectString(full, m.Identity.Name, a.Visual.DisplayName)
		}
		break
	}
	return proc, name
}

// packageFullName returns the full name of the package installed for this user under a family name, the version- and architecture-specific name the other package calls take, or "" when there is none or the call is missing.
func packageFullName(family string) string {
	if procGetPackagesByPackageFamily.Find() != nil {
		return ""
	}
	fam, err := windows.UTF16PtrFromString(family)
	if err != nil {
		return ""
	}
	var count, length uint32
	r, _, _ := procGetPackagesByPackageFamily.Call(uintptr(unsafe.Pointer(fam)), uintptr(unsafe.Pointer(&count)), 0, uintptr(unsafe.Pointer(&length)), 0)
	if r != uintptr(windows.ERROR_INSUFFICIENT_BUFFER) || count == 0 || length == 0 {
		return ""
	}
	names := make([]uintptr, count)
	buf := make([]uint16, length)
	r, _, _ = procGetPackagesByPackageFamily.Call(uintptr(unsafe.Pointer(fam)), uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&names[0])), uintptr(unsafe.Pointer(&length)), uintptr(unsafe.Pointer(&buf[0])))
	if r != 0 {
		return ""
	}
	// The names are written one after another into buf, each ending in a NUL, and names only points into it, so the first is read straight off the front. Any of them will do: one family installed twice for one user differs only by version or architecture.
	return windows.UTF16ToString(buf)
}

// packagePath returns the folder a package is installed in, or "" when it cannot be found.
func packagePath(full string) string {
	if procGetPackagePathByFullName.Find() != nil {
		return ""
	}
	name, err := windows.UTF16PtrFromString(full)
	if err != nil {
		return ""
	}
	var length uint32
	r, _, _ := procGetPackagePathByFullName.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&length)), 0)
	if r != uintptr(windows.ERROR_INSUFFICIENT_BUFFER) || length == 0 {
		return ""
	}
	buf := make([]uint16, length)
	r, _, _ = procGetPackagePathByFullName.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&length)), uintptr(unsafe.Pointer(&buf[0])))
	if r != 0 {
		return ""
	}
	return windows.UTF16ToString(buf)
}

// indirectString returns a manifest string as a person reads it. Input: the package's full name, its Name, and the string, which is either the text itself or an ms-resource: reference into the package's own resources ("ms-resource:AppStoreName" is Sound Recorder's DisplayName). Output: the text, or "" when a reference does not resolve.
// A reference that does not start with a slash is relative to the package's Resources map, though some packages already write it as Resources/...; both readings are tried, the usual one first.
func indirectString(full, pkg, s string) string {
	ref, ok := strings.CutPrefix(strings.TrimSpace(s), "ms-resource:")
	if !ok {
		return strings.TrimSpace(s)
	}
	var uris []string
	switch {
	case strings.HasPrefix(ref, "//"):
		uris = []string{"ms-resource:" + ref}
	case strings.HasPrefix(ref, "/"):
		uris = []string{"ms-resource://" + pkg + ref}
	default:
		uris = []string{"ms-resource://" + pkg + "/Resources/" + ref, "ms-resource://" + pkg + "/" + ref}
	}
	if procSHLoadIndirectString.Find() != nil {
		return ""
	}
	for _, uri := range uris {
		src, err := windows.UTF16PtrFromString("@{" + full + "?" + uri + "}")
		if err != nil {
			continue
		}
		buf := make([]uint16, 512)
		if hr, _, _ := procSHLoadIndirectString.Call(uintptr(unsafe.Pointer(src)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0); hr == 0 {
			if text := strings.TrimSpace(windows.UTF16ToString(buf)); text != "" {
				return text
			}
		}
	}
	return ""
}

// exeDescription returns the FileDescription in an exe's version resource — "Zoom Meetings" for Zoom.exe, "Google Chrome" for chrome.exe — which is the name Task Manager lists a process by. Input: the exe's path. Output: the description, or "" when the exe is gone or carries none, and consentUsers then names it after its file.
func exeDescription(path string) string {
	size, err := windows.GetFileVersionInfoSize(path, nil)
	if err != nil || size == 0 {
		return ""
	}
	info := make([]byte, size)
	if err := windows.GetFileVersionInfo(path, 0, size, unsafe.Pointer(&info[0])); err != nil {
		return ""
	}
	// The strings are filed under a language and code page that the Translation table names; 040904b0 and 040904e4, US English in Unicode and in Windows-1252, are where an exe without one has them.
	var blocks []string
	var trans *[2]uint16
	var n uint32
	if windows.VerQueryValue(unsafe.Pointer(&info[0]), `\VarFileInfo\Translation`, unsafe.Pointer(&trans), &n) == nil && n >= 4 && trans != nil {
		blocks = append(blocks, fmt.Sprintf("%04x%04x", trans[0], trans[1]))
	}
	blocks = append(blocks, "040904b0", "040904e4")
	desc := ""
	for _, b := range blocks {
		var p *uint16
		if windows.VerQueryValue(unsafe.Pointer(&info[0]), `\StringFileInfo\`+b+`\FileDescription`, unsafe.Pointer(&p), &n) == nil && n > 0 && p != nil {
			if desc = strings.TrimSpace(windows.UTF16PtrToString(p)); desc != "" {
				break
			}
		}
	}
	// trans and p point into info, which has to outlive every read through them.
	runtime.KeepAlive(info)
	return desc
}
