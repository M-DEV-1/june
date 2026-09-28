package recorder

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"june/internal/config"
)

// The diarizer is a sherpa-onnx build installed the same way the whisper.cpp one is: the binary, its shared libraries and its two models together under <data dir>/sherpa. It splits the call side of a recording into voices; it never sees the microphone side, which is one known person by construction.
const (
	sherpaBinaryName   = "sherpa-onnx-offline-speaker-diarization"
	sherpaSegmentation = "segmentation-3.0.onnx"
	sherpaEmbedding    = "wespeaker_en_voxceleb_CAM++.onnx"
)

// defaultClusterThreshold is the cosine distance at which two stretches of speech stop being the same voice. It is only ever used when the number of people in the call is unknown, because as a way of getting the speaker count right it does not work: on the 2026-08-31 16:17 recording, a call with exactly one other person in it, thresholds of 0.85, 0.9 and 0.95 all returned four voices, while on a six-person standup the same range swung the answer from 35 down to 6. There is no value that transfers between two meetings, so tuning it is not a path to a correct answer, only to a differently wrong one.
// What does transfer is being told the number outright: given --clustering.num-clusters the diarizer returns exactly that many, so the count is right by construction rather than by calibration. June can read that count off the meeting's own window, which is why this constant is the fallback and not the mechanism.
const defaultClusterThreshold = 0.8

// clusterThreshold returns the configured threshold, or the default when none is set.
func clusterThreshold() string {
	t := config.LoadConfig().Transcribe.ClusterThreshold
	if t <= 0 {
		t = defaultClusterThreshold
	}
	return strconv.FormatFloat(t, 'f', -1, 64)
}

// SherpaBinary returns the path to the diarizer, or an error naming the exact path of each missing file. $JUNE_SHERPA overrides the location.
func SherpaBinary(dataDir string) (string, error) {
	bin := filepath.Join(dataDir, "sherpa", sherpaBinaryName)
	if p := os.Getenv("JUNE_SHERPA"); p != "" {
		bin = p
	}
	models := []string{filepath.Join(filepath.Dir(bin), sherpaSegmentation), filepath.Join(filepath.Dir(bin), sherpaEmbedding)}
	if info, err := os.Stat(bin); err != nil || info.IsDir() {
		return "", fmt.Errorf("no diarizer at %s, and it needs %s beside it", bin, strings.Join(models, " and "))
	}
	for _, m := range models {
		if _, err := os.Stat(m); err != nil {
			return "", fmt.Errorf("no %s at %s, beside the diarizer", filepath.Base(m), m)
		}
	}
	return bin, nil
}

// diarTurnLine matches one line of the diarizer's output, e.g. "0.518 -- 6.032 speaker_01".
var diarTurnLine = regexp.MustCompile(`([0-9.]+)\s*--\s*([0-9.]+)\s+speaker_([0-9]+)`)

// parseDiarTurns pulls the speaker turns out of the diarizer's stdout, ignoring the banner it prints around them.
func parseDiarTurns(out string) []diarTurn {
	var turns []diarTurn
	for _, line := range strings.Split(out, "\n") {
		m := diarTurnLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		start, err1 := strconv.ParseFloat(m[1], 64)
		end, err2 := strconv.ParseFloat(m[2], 64)
		speaker, err3 := strconv.Atoi(m[3])
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		turns = append(turns, diarTurn{
			Start:   time.Duration(start * float64(time.Second)),
			End:     time.Duration(end * float64(time.Second)),
			Speaker: speaker,
		})
	}
	return turns
}

// diarizeWAV runs the diarizer over one WAV and returns the voice turns it found, on the file's own clock.
// speakers is how many voices the caller knows are in the call, from the meeting app's own window; the diarizer then returns exactly that many rather than estimating. Pass 0 when it is not known, and the distance threshold decides instead — which gets the count wrong more often than not.
// offset shifts the turns onto the recording's clock, the same way transcribeWAV shifts its segments.
func diarizeWAV(ctx context.Context, bin, path string, speakers int, offset time.Duration) ([]diarTurn, error) {
	dir := filepath.Dir(bin)
	args := []string{
		"--segmentation.pyannote-model=" + filepath.Join(dir, sherpaSegmentation),
		"--embedding.model=" + filepath.Join(dir, sherpaEmbedding),
		"--segmentation.num-threads=" + strconv.Itoa(transcribeThreads()),
		"--embedding.num-threads=" + strconv.Itoa(transcribeThreads()),
	}
	if speakers > 0 {
		args = append(args, "--clustering.num-clusters="+strconv.Itoa(speakers))
	} else {
		args = append(args, "--clustering.cluster-threshold="+clusterThreshold())
	}
	args = append(args, path)
	out, errOut, err := runWithLibPath(ctx, bin, args, filepath.Join(dir, "lib"))
	if err != nil {
		return nil, fmt.Errorf("diarize %s: %w (%s)", filepath.Base(path), err, strings.TrimSpace(errOut))
	}
	turns := parseDiarTurns(out)
	for i := range turns {
		turns[i].Start += offset
		turns[i].End += offset
	}
	return turns, nil
}

// diarTurn is one span of the call's audio attributed to one clustered voice. Speaker is the cluster number the diarizer assigned, which is stable within a recording and means nothing across recordings — cluster 2 in this meeting is not cluster 2 in the next one.
type diarTurn struct {
	Start, End time.Duration
	Speaker    int
}

// callSpeaker is the label for one clustered voice on the call side, replacing the pooled speakerCall. Numbering starts at one because these are read by a human and by a model, neither of which expects a zeroth speaker.
func callSpeaker(cluster int) string {
	return fmt.Sprintf("%s:S%d", speakerCall, cluster+1)
}

// assignSpeakers relabels each call-side segment with the voice cluster it overlaps most, leaving the microphone side alone — that stream is one known person by construction and there is nothing to work out.
// A segment that overlaps no turn at all keeps the pooled speakerCall label: the diarizer discards speech shorter than its own floor, and a segment it never covered is one it has no opinion about, which is different from one it placed.
// Input: the transcript's segments and the diarizer's turns, both on the recording's clock. Output: the same segments, with call-side speakers named per cluster.
func assignSpeakers(segs []Segment, turns []diarTurn) []Segment {
	for i, s := range segs {
		if s.Speaker != speakerCall {
			continue
		}
		best, bestOverlap := -1, time.Duration(0)
		for _, t := range turns {
			start, end := s.Start, s.End
			if t.Start > start {
				start = t.Start
			}
			if t.End < end {
				end = t.End
			}
			if overlap := end - start; overlap > bestOverlap {
				best, bestOverlap = t.Speaker, overlap
			}
		}
		if best >= 0 {
			segs[i].Speaker = callSpeaker(best)
		}
	}
	return segs
}
