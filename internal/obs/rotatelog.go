package obs

import (
	"fmt"
	"os"
	"sync"
)

// RotatingWriter is an io.Writer over one log file that rolls the file aside once it grows past a size cap, so a daemon that runs for weeks never leaves one ever-growing log behind. It keeps at most three rolled-aside files — path+".1" the newest of them, path+".3" the oldest — and deletes anything older than that.
// Every write and every rotation goes through mu, since slog can be written to from many goroutines at once and a rotation that raced a write could interleave a write into the old file with the new one, or close the file out from under a write in flight.
type RotatingWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	perm     os.FileMode
	file     *os.File
	size     int64
}

// NewRotatingWriter opens (or creates) the log at path and returns a writer that rotates it once it would grow past maxBytes. Input: the path, the size cap in bytes, and the file mode to create and enforce it with. Output: the writer, or an error when the file could not be opened or its mode could not be set.
// The mode is enforced with Chmod even when the file already existed, the same as the plain os.OpenFile call this replaces did, so a log written by an older build that left it world-readable is tightened on the next start.
func NewRotatingWriter(path string, maxBytes int64, perm os.FileMode) (*RotatingWriter, error) {
	w := &RotatingWriter{path: path, maxBytes: maxBytes, perm: perm}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

// open creates or appends to w.path, chmods it to w.perm, and records its current size so the first Write after a restart knows how close to the cap it already is.
func (w *RotatingWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, w.perm)
	if err != nil {
		return fmt.Errorf("open %s: %w", w.path, err)
	}
	if err := f.Chmod(w.perm); err != nil {
		f.Close()
		return fmt.Errorf("secure %s: %w", w.path, err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("stat %s: %w", w.path, err)
	}
	w.file = f
	w.size = info.Size()
	return nil
}

// Write appends p to the log, rotating first when p would push the file past maxBytes. Input: the bytes to write. Output: the number of bytes written and any error — the same contract every io.Writer gives, so this drops straight into slog.NewJSONHandler.
func (w *RotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			// A rotation that failed is not a reason to lose the line: it still goes to whatever file is open, even if that means the cap is missed this once.
			fmt.Fprintf(os.Stderr, "june: could not rotate %s: %v\n", w.path, err)
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

// rotate closes the current file, shifts path.2->path.3, path.1->path.2, path->path.1 (dropping whatever was already at path.3), and reopens path fresh. Called with mu already held.
func (w *RotatingWriter) rotate() error {
	if err := w.file.Close(); err != nil {
		return fmt.Errorf("close %s before rotating: %w", w.path, err)
	}
	// Oldest first, so a failure partway through never leaves two names pointing at the same generation.
	os.Remove(w.path + ".3")
	if err := renameIfExists(w.path+".2", w.path+".3"); err != nil {
		return err
	}
	if err := renameIfExists(w.path+".1", w.path+".2"); err != nil {
		return err
	}
	if err := renameIfExists(w.path, w.path+".1"); err != nil {
		return err
	}
	return w.open()
}

// renameIfExists renames old to new, and does nothing when old does not exist — every step of rotate is optional the first few times a file has not grown enough generations to reach it yet.
func renameIfExists(old, new string) error {
	if _, err := os.Stat(old); os.IsNotExist(err) {
		return nil
	}
	if err := os.Rename(old, new); err != nil {
		return fmt.Errorf("rename %s to %s: %w", old, new, err)
	}
	return nil
}

// Close closes the underlying file.
func (w *RotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.file.Close()
}
