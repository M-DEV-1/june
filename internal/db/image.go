package db

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// maxExtraFrames caps how many non-primary monitors get a frame on disk, because the suffix letters run b..z. Nobody has 26 screens, and a bad caller must not be able to fill the frames dir.
const maxExtraFrames = 25

// extraFrameName is the file name for the i-th extra monitor of an episode: 0 -> "{id}-b.jpg", 1 -> "{id}-c.jpg", and so on. The primary monitor stays "{id}.jpg", so the whole set for an episode is derivable from its id alone and needs no column of its own.
func extraFrameName(id int64, i int) string {
	return fmt.Sprintf("%d-%c.jpg", id, 'b'+rune(i))
}

// writeEpisodeJPEG stores the primary monitor's frame under framesDir/{id}.jpg and each extra monitor's frame beside it as {id}-b.jpg, {id}-c.jpg, then returns the relative path "frames/{id}.jpg" for the primary. Empty if there is no primary image or no frames dir (:memory:).
// Extras are only written when the primary was written: image_path is what the aging queries look for, so an extra with no primary would never be found again and never be reclaimed.
func (s *Store) writeEpisodeJPEG(id int64, jpeg []byte, extras [][]byte) string {
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
	for i, extra := range extras {
		if len(extra) == 0 || i >= maxExtraFrames {
			break
		}
		path := filepath.Join(s.framesDir, extraFrameName(id, i))
		if err := os.WriteFile(path, extra, 0600); err != nil {
			slog.Error("write episode extra monitor jpeg failed", "path", path, "error", err)
			break
		}
	}
	return filepath.ToSlash(filepath.Join("frames", fmt.Sprintf("%d.jpg", id)))
}

// EpisodeExtraImages returns the relative paths of the frames captured from the episode's other monitors, in capture order, or nil when the capture was single-screen or the frames have been aged away. Paths are relative to the store's directory, the same shape as Episode.ImagePath.
func (s *Store) EpisodeExtraImages(id int64) []string {
	if s.framesDir == "" {
		return nil
	}
	var out []string
	for i := 0; i < maxExtraFrames; i++ {
		name := extraFrameName(id, i)
		if _, err := os.Stat(filepath.Join(s.framesDir, name)); err != nil {
			break
		}
		out = append(out, filepath.ToSlash(filepath.Join("frames", name)))
	}
	return out
}

// removeEpisodeJPEG deletes every monitor's frame for the episode. Every path that ages or prunes an episode goes through here, so an extra monitor's frame can never outlive the primary it was captured with.
func (s *Store) removeEpisodeJPEG(id int64) {
	if s.framesDir == "" {
		return
	}
	_ = os.Remove(filepath.Join(s.framesDir, fmt.Sprintf("%d.jpg", id)))
	for i := 0; i < maxExtraFrames; i++ {
		if err := os.Remove(filepath.Join(s.framesDir, extraFrameName(id, i))); err != nil {
			break
		}
	}
}
