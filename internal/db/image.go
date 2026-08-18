package db

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// writeEpisodeJPEG stores jpeg under framesDir/{id}.jpg and returns the relative path "frames/{id}.jpg". Empty if there is no image or no frames dir (:memory:).
func (s *Store) writeEpisodeJPEG(id int64, jpeg []byte) string {
	if len(jpeg) == 0 || s.framesDir == "" {
		return ""
	}
	if err := os.MkdirAll(s.framesDir, 0700); err != nil {
		slog.Error("create frames dir failed", "dir", s.framesDir, "error", err)
		return ""
	}
	abs := filepath.Join(s.framesDir, fmt.Sprintf("%d.jpg", id))
	if err := os.WriteFile(abs, jpeg, 0600); err != nil {
		slog.Error("write episode jpeg failed", "path", abs, "error", err)
		return ""
	}
	return filepath.ToSlash(filepath.Join("frames", fmt.Sprintf("%d.jpg", id)))
}

func (s *Store) removeEpisodeJPEG(id int64) {
	if s.framesDir == "" {
		return
	}
	_ = os.Remove(filepath.Join(s.framesDir, fmt.Sprintf("%d.jpg", id)))
}
