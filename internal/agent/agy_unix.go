//go:build !windows

package agent

import "os"

// linkEntry makes dst a symbolic link to src in the throwaway HOME. A directory linked this way is the real directory under a second name, so a SQLite database agy opens through it keeps its -wal and -shm beside the real file.
func linkEntry(src, dst string) error {
	return os.Symlink(src, dst)
}
