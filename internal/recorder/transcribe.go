package recorder

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// whisperCandidates are the whisperfile names looked for under <data dir>/whisper, best first. These are single-file executables from Mozilla's whisperfile release with the weights baked in, so there is no separate model file to point at.
// The multilingual builds come first because the meetings being recorded are Hindi-English code-switched, and an .en build hears that as nothing at all — a 16-minute call came back with no speech in it. The multilingual models detect the language themselves and transcribe it as spoken, so no language flag is passed and --translate is never used.
var whisperCandidates = []string{
	"whisper-medium.llamafile",
	"whisper-small.llamafile",
	"whisper-medium.en.llamafile",
	"whisper-small.en.llamafile",
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
	started := time.Now()
	out, errOut, err := run(ctx, bin, args)
	if errors.Is(err, syscall.ENOEXEC) {
		// A whisperfile is an APE binary: the kernel refuses it without a binfmt_misc registration, but its header doubles as a shell script that loads the real executable. A plain ELF whisper build takes the direct path above instead.
		started = time.Now()
		out, errOut, err = run(ctx, "/bin/sh", append([]string{bin}, args...))
	}
	took := time.Since(started)
	// Whisper's own account of the run used to be thrown away, which is why two meetings that failed outright looked like meetings nobody spoke in. Keep it wherever the run ends up.
	slog.Debug("whisper finished", "file", filepath.Base(path), "took", took, "stderr", strings.TrimSpace(errOut))
	if err != nil {
		return nil, fmt.Errorf("whisper %s: %w (%s)", filepath.Base(path), err, strings.TrimSpace(errOut))
	}
	// whisperfile exits 0 whether or not it managed to decode the audio, so a file it could not read is indistinguishable from a meeting nobody spoke in unless its stderr is read.
	if strings.Contains(errOut, "failed to read") {
		return nil, fmt.Errorf("whisper could not read %s: %s", filepath.Base(path), strings.TrimSpace(lineContaining(errOut, "failed to read")))
	}

	segs := parseSegments(out, speaker, offset)
	// Whisper runs at a few times real time, so a long recording that came back with nothing in a moment was never listened to: whatever went wrong, it exited 0 without saying so. Treating that as a silent meeting is what kept the audio's real contents hidden.
	if audio := wavDuration(path); len(segs) == 0 && audio >= minAudioForSpeedCheck && took < audio/whisperSlowestPlausibleSpeed {
		slog.Warn("whisper produced nothing far too quickly to have transcribed the audio", "file", filepath.Base(path), "audio", audio, "took", took, "stderr", strings.TrimSpace(errOut))
		return nil, fmt.Errorf("whisper returned no speech for %s after %s of %s of audio, which is too fast to be a transcription: %s", filepath.Base(path), took.Round(time.Millisecond), audio.Round(time.Second), strings.TrimSpace(errOut))
	}
	return segs, nil
}

// whisperSlowestPlausibleSpeed is the slowest ratio of audio length to run time that still counts as a real transcription. small on this CPU runs around four times real time, so a run twenty times faster than the audio it was given did not decode it.
const whisperSlowestPlausibleSpeed = 20

// minAudioForSpeedCheck is the shortest recording the speed check applies to. A few seconds of audio genuinely does transcribe in a blink, and a fixed process startup cost swamps the ratio there.
const minAudioForSpeedCheck = 30 * time.Second

// wavDuration returns how much 16 kHz mono 16-bit audio a WAV file holds, from its size. Returns zero if the file cannot be measured.
func wavDuration(path string) time.Duration {
	info, err := os.Stat(path)
	if err != nil || info.Size() <= wavHeaderSize {
		return 0
	}
	frames := (info.Size() - wavHeaderSize) / 2
	return time.Duration(frames) * time.Second / sampleRate
}

// run executes one command and returns its stdout and stderr separately, so a tool that reports failure on stderr while exiting 0 can still be caught.
func run(ctx context.Context, name string, args []string) (stdout, stderr string, err error) {
	var out, errOut strings.Builder
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err = cmd.Run()
	return out.String(), errOut.String(), err
}

// lineContaining returns the first line of s that contains want, so an error message can quote the complaint rather than the whole model-loading banner.
func lineContaining(s, want string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, want) {
			return line
		}
	}
	return s
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
