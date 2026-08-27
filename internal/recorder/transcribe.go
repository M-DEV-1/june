package recorder

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Segment is one span of speech from one side of the call, with its start and end measured from the beginning of the recording (not from the beginning of its own stream).
type Segment struct {
	Start, End time.Duration
	Speaker    string // speakerMe for the microphone, speakerCall for the system audio
	Text       string
}

// The two labels a transcript line can carry. speakerCall is deliberately neutral rather than "them": every remote voice is pooled into it here, and a later diarization pass is meant to split it into named speakers in place.
const (
	speakerMe   = "me"
	speakerCall = "call"
)

// whisperCandidates are the whisperfile names looked for under <data dir>/whisper, best quality first. These are single-file executables from Mozilla's whisperfile release with the weights baked in, so there is no separate model file to point at.
var whisperCandidates = []string{
	"whisper-small.en.llamafile",
	"whisper-medium.en.llamafile",
	"whisper-tiny.en.llamafile",
}

// whisperBinary returns the path to the whisper executable to run: $ORA_WHISPER if set, otherwise the best whisperCandidates entry present under dataDir/whisper.
func whisperBinary(dataDir string) (string, error) {
	if p := os.Getenv("ORA_WHISPER"); p != "" {
		return p, nil
	}
	dir := filepath.Join(dataDir, "whisper")
	for _, name := range whisperCandidates {
		p := filepath.Join(dir, name)
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("no whisperfile found in %s (expected one of %s) and $ORA_WHISPER is unset", dir, strings.Join(whisperCandidates, ", "))
}

// transcribeWAV runs whisper over one WAV and returns its segments, labelled with speaker and shifted by offset — how much later this stream started than the recording as a whole.
// It repairs the WAV header first, so a file left behind by a crashed daemon still transcribes.
func transcribeWAV(ctx context.Context, bin, path, speaker string, offset time.Duration) ([]Segment, error) {
	if err := repairWAV(path); err != nil {
		return nil, fmt.Errorf("repair %s: %w", path, err)
	}
	args := []string{"-f", path, "-np"}
	out, err := exec.CommandContext(ctx, bin, args...).Output()
	if errors.Is(err, syscall.ENOEXEC) {
		// A whisperfile is an APE binary: the kernel refuses it without a binfmt_misc registration, but its header doubles as a shell script that loads the real executable. A plain ELF whisper build takes the direct path above instead.
		out, err = exec.CommandContext(ctx, "/bin/sh", append([]string{bin}, args...)...).Output()
	}
	if err != nil {
		return nil, fmt.Errorf("whisper %s: %w", filepath.Base(path), err)
	}
	return parseSegments(string(out), speaker, offset), nil
}

// segmentLine matches whisperfile's per-segment stdout line, e.g. "[00:00:07.640 --> 00:00:17.840]   text here".
var segmentLine = regexp.MustCompile(`^\[(\d+):(\d+):(\d+)\.(\d+) --> (\d+):(\d+):(\d+)\.(\d+)\]\s*(.*)$`)

// parseSegments pulls the timestamped segments out of whisper's stdout, dropping banner lines and segments with no text. Every timestamp is shifted by offset so segments from both streams share one clock.
func parseSegments(out, speaker string, offset time.Duration) []Segment {
	var segs []Segment
	for _, line := range strings.Split(out, "\n") {
		m := segmentLine.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil {
			continue
		}
		text := strings.TrimSpace(m[9])
		if text == "" || nonSpeech.MatchString(text) {
			continue
		}
		segs = append(segs, Segment{
			Start:   hmsToDuration(m[1], m[2], m[3], m[4]) + offset,
			End:     hmsToDuration(m[5], m[6], m[7], m[8]) + offset,
			Speaker: speaker,
			Text:    text,
		})
	}
	return segs
}

// nonSpeech matches whisper's markers for a stretch with no words in it — "[BLANK_AUDIO]", "(upbeat music)", "[SOUND]". A silent stream is otherwise nothing but these, and the microphone side of a call is silent most of the time.
var nonSpeech = regexp.MustCompile(`^[\[(][^)\]]*[)\]]$`)

// hmsToDuration turns one whisper timestamp into a duration. frac is the fractional-seconds digits exactly as printed, so its length is its scale: "64" means 640ms, "640" means 640ms, and anything past three digits is finer than this cares about and is cut.
func hmsToDuration(h, m, s, frac string) time.Duration {
	n := func(v string) int { i, _ := strconv.Atoi(v); return i }
	if len(frac) > 3 {
		frac = frac[:3]
	}
	ms := n(frac)
	for i := len(frac); i < 3; i++ {
		ms *= 10
	}
	return time.Duration(n(h))*time.Hour + time.Duration(n(m))*time.Minute + time.Duration(n(s))*time.Second + time.Duration(ms)*time.Millisecond
}

// renderTranscript sorts segments from both streams into one chronological conversation, one line per segment: "[MM:SS] [me] what was said".
func renderTranscript(segs []Segment) string {
	sort.SliceStable(segs, func(i, j int) bool { return segs[i].Start < segs[j].Start })
	var b strings.Builder
	for _, s := range segs {
		total := int(s.Start.Seconds())
		fmt.Fprintf(&b, "[%02d:%02d:%02d] [%s] %s\n", total/3600, total/60%60, total%60, s.Speaker, s.Text)
	}
	return b.String()
}
