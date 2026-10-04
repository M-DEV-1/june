package ipc

import (
	"errors"
	"runtime"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// microphoneConsentKey is where Windows keeps the microphone privacy switches, under both HKCU and HKLM. The key's own Value is the user's "Microphone access" master switch (HKCU) or the device-wide one an administrator sets (HKLM), and its NonPackaged subkey is "Let desktop apps access your microphone", the one that names june.exe's kind of app.
const microphoneConsentKey = `Software\Microsoft\Windows\CurrentVersion\CapabilityAccessManager\ConsentStore\microphone`

// micConsent reads whether Windows lets a desktop app like June hear the microphone. A blocked app is not refused by WASAPI: its stream opens and delivers silence, so this is what tells the user a silent test is Windows' doing rather than the microphone's. The switches that cover june.exe are the device-wide one under HKLM, which stops every app, the user's own master switch under HKCU, and "Let desktop apps access your microphone" (NonPackaged) under either hive.
// The LetAppsAccessMicrophone policy governs Windows (packaged) apps, so its Force Deny (2) says nothing about June. Its Force Allow (1) does override the user's master switch, which Settings then greys out, so that switch is not read under it: a stale Deny there would send the user to a page where nothing can be changed.
// Output: "blocked" when a switch that covers June says no, "allowed" when at least one was read and none says no, "unknown" when none could be read. The mic test overrides "blocked" with anything it heard.
func micConsent() string {
	forceAllow := false
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Policies\Microsoft\Windows\AppPrivacy`, registry.QUERY_VALUE); err == nil {
		v, _, err := k.GetIntegerValue("LetAppsAccessMicrophone")
		k.Close()
		forceAllow = err == nil && v == 1
	}
	type consentSwitch struct {
		root registry.Key
		path string
	}
	switches := []consentSwitch{
		{registry.LOCAL_MACHINE, microphoneConsentKey},
		{registry.LOCAL_MACHINE, microphoneConsentKey + `\NonPackaged`},
		{registry.CURRENT_USER, microphoneConsentKey + `\NonPackaged`},
	}
	if !forceAllow {
		switches = append(switches, consentSwitch{registry.CURRENT_USER, microphoneConsentKey})
	}
	read := false
	for _, sw := range switches {
		value, ok := registryString(sw.root, sw.path, "Value")
		if !ok {
			continue
		}
		read = true
		if value == "Deny" {
			return "blocked"
		}
	}
	if !read {
		return "unknown"
	}
	return "allowed"
}

// registryString reads one string value. Output: the value and true, or false when the key or the value is missing or is not a string.
func registryString(root registry.Key, path, name string) (string, bool) {
	k, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
	if err != nil {
		return "", false
	}
	defer k.Close()
	v, _, err := k.GetStringValue(name)
	return v, err == nil
}

// propVariantClear frees what a PROPVARIANT holds. go-ole's VARIANT.Clear calls VariantClear, which does not know the VT_LPWSTR a device's friendly name comes back as.
var propVariantClear = windows.NewLazySystemDLL("ole32.dll").NewProc("PropVariantClear")

// defaultMicName is the name Windows shows for the default capture device in the console role, the device audio.NewMic records from. It only reads the device's properties and never opens a stream. Output: the name, or "" when there is no capture device or COM would not answer.
func defaultMicName() string {
	name := make(chan string, 1)
	// COM objects belong to the thread that made them, so the whole read runs on one locked thread, as internal/audio does for its streams.
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); err != nil {
			var oe *ole.OleError
			// S_FALSE means COM was already initialised on this thread, which still wants the matching CoUninitialize.
			if !errors.As(err, &oe) || oe.Code() != 1 {
				name <- ""
				return
			}
		}
		defer ole.CoUninitialize()
		name <- endpointName()
	}()
	return <-name
}

// endpointName does defaultMicName's read on a thread with COM already initialised.
func endpointName() string {
	var de *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL, wca.IID_IMMDeviceEnumerator, &de); err != nil {
		return ""
	}
	defer de.Release()
	var dev *wca.IMMDevice
	if err := de.GetDefaultAudioEndpoint(wca.ECapture, wca.EConsole, &dev); err != nil {
		return ""
	}
	defer dev.Release()
	var ps *wca.IPropertyStore
	if err := dev.OpenPropertyStore(wca.STGM_READ, &ps); err != nil {
		return ""
	}
	defer ps.Release()
	var pv wca.PROPVARIANT
	if err := ps.GetValue(&wca.PKEY_Device_FriendlyName, &pv); err != nil {
		return ""
	}
	defer propVariantClear.Call(uintptr(unsafe.Pointer(&pv)))
	// Read here rather than with pv.String, which dereferences Val whatever the type, so a device with no name (VT_EMPTY, Val 0) would crash the daemon, and frees the string itself behind PropVariantClear's back.
	if pv.VT != ole.VT_LPWSTR {
		return ""
	}
	return windows.UTF16PtrToString(*(**uint16)(unsafe.Pointer(&pv.Val)))
}

// openMicSettings opens Windows' microphone privacy page, where the switches micConsent reads are turned back on. The URI is fixed here and never comes from a request, which is why this is its own route and POST /open stays limited to http and https. ShellExecute rather than a child process, so no console flashes up from the windowless daemon.
// ms-settings: belongs to a packaged app, which the shell activates through COM, so the call is made as ShellExecute's documentation asks: on one OS thread, with COM initialised single-threaded and OLE1 DDE off, as internal/agent's launchEntry does for shortcuts. A thread some earlier code left multithreaded refuses that (RPC_E_CHANGED_MODE) but has COM all the same, so the call goes ahead there without an uninitialise of its own.
func openMicSettings() error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED|windows.COINIT_DISABLE_OLE1DDE); err == nil || err == windows.Errno(windows.S_FALSE) {
		defer windows.CoUninitialize()
	}
	verb, _ := windows.UTF16PtrFromString("open")
	target, _ := windows.UTF16PtrFromString("ms-settings:privacy-microphone")
	letOpenedComeForward()
	return windows.ShellExecute(0, verb, target, nil, nil, windows.SW_SHOWNORMAL)
}
