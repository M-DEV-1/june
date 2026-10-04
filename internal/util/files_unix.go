//go:build !windows

package util

import "os"

// renameOver moves src over dst. A rename here replaces dst whoever has it open.
func renameOver(src, dst string) error {
	return os.Rename(src, dst)
}

// keepPrivate has nothing to add outside Windows: the mode WriteFilePrivate sets is the whole of a file's permissions here.
func keepPrivate(path string) error { return nil }

// protectOutsideProfile has nothing to add outside Windows: MkdirPrivate's 0700 already keeps the folder to its owner.
func protectOutsideProfile(dir string) {}
