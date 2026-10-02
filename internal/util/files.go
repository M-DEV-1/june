package util

import (
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

// WriteFileAtomic replaces path with data so a crash or power loss mid-write can never leave a short or half-written file behind for a later read to choke on. Input: the destination path, the bytes to write, and the permission to create it with. Output: the first error from creating the temp file, writing to it, closing it, or renaming it over path; the temp file is removed on any of those failures.
// It writes to a temp file created in path's own directory rather than to path+".tmp" beside it, which guarantees the temp file lands on the same filesystem as path (so the final rename is one filesystem operation, not a copy) and gives the temp file a unique name (so two concurrent writers to the same path never collide on the same temp file).
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
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
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}
