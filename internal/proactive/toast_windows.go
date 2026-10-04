//go:build windows

package proactive

import (
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// startMenuPrograms is the user's Start-menu Programs folder, which is where the installer puts June's shortcut ({userprograms}). It is asked of the shell rather than built from %APPDATA%, because the Start menu can be redirected on its own; the %APPDATA% path is only the fallback when the shell does not answer. Output: the folder, or "" when neither is known.
func startMenuPrograms() string {
	if dir, err := windows.KnownFolderPath(windows.FOLDERID_Programs, 0); err == nil {
		return dir
	}
	if appData := os.Getenv("APPDATA"); appData != "" {
		return filepath.Join(appData, "Microsoft", "Windows", "Start Menu", "Programs")
	}
	return ""
}

var (
	shell32                               = windows.NewLazySystemDLL("shell32.dll")
	procSHGetPropertyStoreFromParsingName = shell32.NewProc("SHGetPropertyStoreFromParsingName")
	procPropVariantClear                  = windows.NewLazySystemDLL("ole32.dll").NewProc("PropVariantClear")

	// IID_IPropertyStore, and PKEY_AppUserModel_ID from propkey.h.
	iidPropertyStore  = windows.GUID{Data1: 0x886D8EEB, Data2: 0x8CF2, Data3: 0x4446, Data4: [8]byte{0x8D, 0x02, 0xCD, 0xBA, 0x1D, 0xBD, 0xCF, 0x99}}
	appUserModelIDKey = propertyKey{fmtid: windows.GUID{Data1: 0x9F4C2855, Data2: 0x9F79, Data3: 0x4B39, Data4: [8]byte{0xA8, 0xD0, 0xE1, 0xD4, 0x2D, 0xE1, 0xD5, 0xF3}}, pid: 5}
)

const (
	gpsDefault = 0
	vtLPWStr   = 31
	// The IPropertyStore methods called, by their place in its vtable after IUnknown's three.
	propertyStoreRelease  = 2
	propertyStoreGetValue = 5
	sFalse                = syscall.Errno(1)
	rpcEChangedMode       = syscall.Errno(0x80010106)
	// shortcutReadTimeout bounds reading a shortcut, after which the toast goes out under PowerShell's ID rather than not at all.
	shortcutReadTimeout = 5 * time.Second
)

// propertyKey is PROPERTYKEY.
type propertyKey struct {
	fmtid windows.GUID
	pid   uint32
}

// propVariant is PROPVARIANT: a type, three reserved words, and a union as wide as two pointers on 64-bit Windows and as 8 bytes on 32-bit, whose first word is a string's pointer.
type propVariant struct {
	vt  uint16
	_   [3]uint16
	val uintptr
	_   uintptr
}

// propertyStore is a COM IPropertyStore, called through its vtable.
type propertyStore struct{ vtbl *[8]uintptr }

// shortcutAppID reads the AppUserModelID a shortcut carries, from the shell's property store for the file, which is where Windows reads it when it matches a toast to a shortcut. Input: the .lnk. Output: the ID, or "" when there is no such file, it carries none, or it cannot be read in time.
func shortcutAppID(lnk string) string {
	if _, err := os.Stat(lnk); err != nil {
		return ""
	}
	id := make(chan string, 1)
	// COM is set up per thread, so the read has a thread to itself for as long as it runs.
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		id <- readShortcutAppID(lnk)
	}()
	select {
	case s := <-id:
		return s
	case <-time.After(shortcutReadTimeout):
		return ""
	}
}

// readShortcutAppID is shortcutAppID's read, on a thread locked for it.
func readShortcutAppID(lnk string) string {
	switch err := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED|windows.COINIT_DISABLE_OLE1DDE); err {
	case nil, sFalse:
		defer windows.CoUninitialize()
	case rpcEChangedMode:
		// The thread was left in the multithreaded apartment by whatever had it before, which serves this read as well; undoing that is not this function's to do.
	default:
		return ""
	}
	path, err := windows.UTF16PtrFromString(lnk)
	if err != nil {
		return ""
	}
	var store *propertyStore
	if hr, _, _ := procSHGetPropertyStoreFromParsingName.Call(uintptr(unsafe.Pointer(path)), 0, gpsDefault, uintptr(unsafe.Pointer(&iidPropertyStore)), uintptr(unsafe.Pointer(&store))); hr != 0 || store == nil {
		return ""
	}
	defer syscall.SyscallN(store.vtbl[propertyStoreRelease], uintptr(unsafe.Pointer(store)))
	var v propVariant
	if hr, _, _ := syscall.SyscallN(store.vtbl[propertyStoreGetValue], uintptr(unsafe.Pointer(store)), uintptr(unsafe.Pointer(&appUserModelIDKey)), uintptr(unsafe.Pointer(&v))); hr != 0 {
		return ""
	}
	defer procPropVariantClear.Call(uintptr(unsafe.Pointer(&v)))
	if v.vt != vtLPWStr {
		return ""
	}
	return windows.UTF16PtrToString(*(**uint16)(unsafe.Pointer(&v.val)))
}
