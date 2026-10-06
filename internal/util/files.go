package util

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
)

// Exists reports whether anything is at path.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// DataHome returns %LOCALAPPDATA% on Windows, and elsewhere $XDG_DATA_HOME, or ~/.local/share when it is unset. Output: the directory, or "" when none of these can be found.
// The window resolves the same directory in data_dir (app/src-tauri/src/lib.rs), and the two must agree because they share the IPC token.
func DataHome() string {
	if runtime.GOOS == "windows" {
		// os.UserCacheDir is %LOCALAPPDATA% on Windows.
		dir, _ := os.UserCacheDir()
		return dir
	}
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share")
}

// WriteFileAtomic replaces path with data so a crash or power loss mid-write can never leave a short or half-written file behind for a later read to choke on. Input: the destination path, the bytes to write, and the permission to create it with. Output: the first error from creating the temp file, writing to it, flushing it, closing it, or renaming it over path; the temp file is removed on any of those failures.
// It writes to a temp file created in path's own directory rather than to path+".tmp" beside it, which guarantees the temp file lands on the same filesystem as path (so the final rename is one filesystem operation, not a copy) and gives the temp file a unique name (so two concurrent writers to the same path never collide on the same temp file).
// On Windows the file takes its folder's permissions whatever perm says, which is right for a file another program owns (a CLI's own login file June refreshes) and for June's state in its own folder, which MkdirPrivate has already kept to the user. WriteFilePrivate is for a file that must be the user's alone wherever it is.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	return writeAtomic(path, data, perm, false)
}

// WriteFilePrivate is WriteFileAtomic for one of June's own files that must be the user's alone, the env file with the keys and june-config.json: mode 0600, and on Windows, where the mode means nothing and a file outside the profile takes the drive's "every signed-in user may read" rule, a DACL of the user and SYSTEM alone, set before anything is written. Never for another program's file, whose permissions are that program's to choose. Input: the destination path and the bytes. Output: as WriteFileAtomic.
func WriteFilePrivate(path string, data []byte) error {
	return writeAtomic(path, data, 0o600, true)
}

// writeAtomic is WriteFileAtomic, with private saying whether the file is also given an owner-only DACL (see WriteFilePrivate).
func writeAtomic(path string, data []byte, perm os.FileMode, private bool) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if private {
		// A filesystem with no ACLs (FAT, some network shares) cannot do this, and the write must still happen there.
		if err := keepPrivate(tmpPath); err != nil {
			slog.Debug("could not make a file private to the user", "file", path, "error", err)
		}
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	// Flushed before the rename: the rename is journalled and the data is not, so after a power cut a rename that reached the disk before the data could leave june-config.json full of zeros, which no later read can parse.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := renameOver(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

// MkdirPrivate makes dir and any parents missing, for June's own state. 0700 is all it takes outside Windows. On Windows the mode means nothing, and a folder made outside the user's profile (JUNE_DATA_DIR on another drive, say) takes that drive's "Authenticated Users may modify" rule, which would let another account on the machine read the IPC token and the keys; such a folder is given a DACL of its own (see protectOutsideProfile). Output: the error from making it; a DACL that cannot be set is logged, not returned.
func MkdirPrivate(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	protectOutsideProfile(dir)
	return nil
}
