package util

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

// renameOver moves src over dst, retrying for up to about a second while dst is held open. On Windows the replace fails with ERROR_ACCESS_DENIED or ERROR_SHARING_VIOLATION for as long as anyone has dst open without FILE_SHARE_DELETE, and Go's own os.Open and os.ReadFile never pass it, so a LoadConfig reading june-config.json at the moment SaveConfig replaces it was enough to lose the save (reproduced 2026-10-03 with os.Open: "Access is denied."). Defender and the search indexer open freshly written files the same way. Those holds last milliseconds, so a short wait gets through them; one that outlasts it returns the last error.
func renameOver(src, dst string) error {
	delay := 10 * time.Millisecond
	for attempt := 0; ; attempt++ {
		err := os.Rename(src, dst)
		if err == nil || attempt == 7 || !(errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_LOCK_VIOLATION)) {
			return err
		}
		time.Sleep(delay)
		delay = min(delay*2, 250*time.Millisecond)
	}
}

// protectTried holds the folders, lowercased absolute paths, protectOutsideProfile has already dealt with in this process.
var protectTried sync.Map

// fileAllAccess is FILE_ALL_ACCESS, every right on a file or folder, which x/sys/windows has no name for. The specific rights are written rather than GENERIC_ALL so the entry reads as Full control wherever it is shown, as the ones Windows writes itself do.
const fileAllAccess = 0x1F01FF

// keepPrivate gives path a DACL that lets in only the user this process runs as and SYSTEM, with nothing inherited from its folder, which is what mode 0600 means on Linux. Input: a file. Output: the error from reading this process's user or setting the DACL.
func keepPrivate(path string) error {
	return ownerOnly(path, windows.NO_INHERITANCE)
}

// ownerOnly sets path's DACL to two entries, the user this process runs as and SYSTEM, both with every right, protected from whatever its parent folder passes down. Input: the file or folder, and how the entries pass on to what is inside it. Output: the error, if any.
func ownerOnly(path string, inherit uint32) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{
		{
			AccessPermissions: fileAllAccess,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       inherit,
			Trustee:           windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_USER, TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid)},
		},
		{
			AccessPermissions: fileAllAccess,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       inherit,
			Trustee:           windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_WELL_KNOWN_GROUP, TrusteeValue: windows.TrusteeValueFromSID(system)},
		},
	}, nil)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}

// protectOutsideProfile makes dir private to the user when it is outside the user's profile and has not been made so already. Inside the profile the folder already inherits a DACL naming only the user, SYSTEM and Administrators, and is left alone. The check for an existing protected DACL keeps this to once per folder: setting it walks everything already inside to pass the new entries down, which for a folder of recordings and models is not something to repeat on every save. Input: the folder. Output: none; a failure is logged.
func protectOutsideProfile(dir string) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return
	}
	// Once per folder per process, whatever came of it. A volume that keeps no DACL (exFAT, FAT32, some shares) never shows a protected one, so without this every save retried the whole walk and logged the same warning again.
	if _, tried := protectTried.LoadOrStore(strings.ToLower(abs), true); tried {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return
	}
	if rel, err := filepath.Rel(home, abs); err == nil && rel != ".." && !strings.HasPrefix(rel, `..\`) && !filepath.IsAbs(rel) {
		return
	}
	if sd, err := windows.GetNamedSecurityInfo(abs, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION); err == nil {
		if control, _, err := sd.Control(); err == nil && control&windows.SE_DACL_PROTECTED != 0 {
			return
		}
	}
	if err := ownerOnly(abs, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT); err != nil {
		slog.Warn("June's folder is outside your profile and could not be made private to you; other accounts on this PC may be able to read it", "dir", abs, "error", err)
		return
	}
	slog.Info("made June's folder, which is outside your profile, private to you", "dir", abs)
}
