package components

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"june/internal/config"
	"june/internal/recorder"
	"june/internal/util"
)

// receipt records one component June installed: which build, its hash, and every file it put in place, relative to the data directory with forward slashes. Remove deletes exactly these files and nothing else, which is also how a file put there by hand is told apart from one June owns.
type receipt struct {
	ID          string    `json:"id"`
	Variant     string    `json:"variant"`
	SHA256      string    `json:"sha256"`
	Files       []string  `json:"files"`
	InstalledAt time.Time `json:"installed_at"`
}

// stateFile is <data>/components/state.json. Failed keeps the last error of a feature or component whose install did not finish, so the window still says so after a restart; Trash lists files that were in use when June tried to delete them, for the next sweep to finish; Pending is the job queue, running job first, so a quit, an update or a crash in the middle of a download takes it up again from its partial file at the next start instead of dropping it while the window still shows it.
type stateFile struct {
	Components []receipt         `json:"components"`
	Failed     map[string]string `json:"failed,omitempty"`
	Trash      []string          `json:"trash,omitempty"`
	Pending    []pendingJob      `json:"pending,omitempty"`
	NoCUDA     *cudaMark         `json:"no_cuda,omitempty"`
}

// cudaMark is the card and driver the CUDA whisper build failed its test on. While the machine has that card on that driver, "auto" does not choose CUDA; a driver update, the usual fix, clears it by no longer matching.
type cudaMark struct {
	GPU    string `json:"gpu"`
	Driver string `json:"driver"`
}

// pendingJob is one queued or running install as state.json keeps it: the id asked for, the build chosen for it, and the build it replaces, whose leftovers are deleted once the new one works. Starts counts the daemon starts that took the job up running, with no clean stop and no headway in between, so a job that takes the daemon down with it is given up on rather than retried at every start. Mark is how many of the job's bytes were on disk when it was last queued or taken up, which tells the next start whether the run in between got anywhere.
type pendingJob struct {
	ID       string `json:"id"`
	Variant  string `json:"variant"`
	Replaces string `json:"replaces,omitempty"`
	Starts   int    `json:"starts,omitempty"`
	Mark     int64  `json:"mark,omitempty"`
}

func (s *Service) statePath() string {
	return filepath.Join(s.opts.DataDir, "components", "state.json")
}

// loadState reads state.json. A missing file is an empty state; one that will not parse is treated the same, because the files it described are still on disk and still found as external, so nothing June relies on is lost with it.
func (s *Service) loadState() stateFile {
	var st stateFile
	b, err := os.ReadFile(s.statePath())
	if err == nil {
		json.Unmarshal(b, &st)
	}
	if st.Failed == nil {
		st.Failed = map[string]string{}
	}
	return st
}

func (s *Service) saveState(st stateFile) error {
	if err := os.MkdirAll(filepath.Dir(s.statePath()), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return util.WriteFileAtomic(s.statePath(), b, 0o600)
}

// editState reads, changes and writes state.json under the state lock. Input: the change. Output: the write's error.
func (s *Service) editState(fn func(*stateFile)) error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	st := s.loadState()
	fn(&st)
	return s.saveState(st)
}

// receipts is the current receipts by component id.
func (s *Service) receipts() map[string]receipt {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	out := map[string]receipt{}
	for _, r := range s.loadState().Components {
		out[r.ID] = r
	}
	return out
}

// abs turns a receipt's relative path into a path on disk.
func (s *Service) abs(rel string) string {
	return filepath.Join(s.opts.DataDir, filepath.FromSlash(rel))
}

// partStatus is how far one part of a feature is installed.
type partStatus int

const (
	partMissing  partStatus = iota // no receipt for this build, or one of its files is gone
	partOutdated                   // June installed this build, but the manifest now pins a different file
	partCurrent                    // installed and pinned to exactly this file
)

// status checks one part against its receipt and the files on disk.
func (s *Service) status(pt part, recs map[string]receipt) partStatus {
	r, ok := recs[pt.ID]
	if !ok || r.Variant != pt.Variant {
		return partMissing
	}
	for _, f := range r.Files {
		if _, err := os.Lstat(s.abs(f)); err != nil {
			return partMissing
		}
	}
	if r.SHA256 != pt.Art.SHA256 {
		return partOutdated
	}
	return partCurrent
}

// installedVariant finds the variant of a feature whose every part has a receipt, preferring one whose needs this machine meets, since two variants can share the same builds (llama-server's Windows zip is both the Vulkan and the CPU one). Output: the variant, its parts, whether any part is pinned to a newer file than the one installed, and false when no variant is wholly installed.
func (s *Service) installedVariant(f *feature, p Platform, recs map[string]receipt) (featureVariant, []part, bool, bool) {
	var fallback *featureVariant
	var fallbackParts []part
	fallbackOld := false
	for _, v := range f.variantsHere() {
		parts, ok := s.man.parts(v, p)
		if !ok {
			continue
		}
		whole, old := true, false
		for _, pt := range parts {
			switch s.status(pt, recs) {
			case partMissing:
				whole = false
			case partOutdated:
				old = true
			}
		}
		if !whole {
			continue
		}
		if p.meets(v.Needs) {
			return v, parts, old, true
		}
		if fallback == nil {
			v := v
			fallback, fallbackParts, fallbackOld = &v, parts, old
		}
	}
	if fallback != nil {
		return *fallback, fallbackParts, fallbackOld, true
	}
	return featureVariant{}, nil, false, false
}

// usable reports whether the daemon would use a feature right now, by asking the same lookups the daemon itself makes. A feature that is usable with no receipt behind it was put there by hand: June reports it as external and will neither overwrite nor delete it.
func (s *Service) usable(id string) bool {
	cfg, _ := config.ReadConfig()
	switch id {
	case "transcribe":
		_, err := recorder.WhisperCPPBinary(s.opts.DataDir)
		return err == nil
	case "speakers":
		_, err := recorder.SherpaBinary(s.opts.DataDir)
		return err == nil
	case "memory":
		return cfg.Embed.LocalEnabled() && util.Exists(cfg.Embed.LlamaServer) && util.Exists(cfg.Embed.ModelPath)
	case "summaries":
		if !cfg.LocalText.Enabled(cfg) || !util.Exists(cfg.LocalText.ResolvedModelPath(cfg)) {
			return false
		}
		bin := cfg.LocalText.ResolvedBinary(cfg)
		if util.Exists(bin) {
			return true
		}
		_, err := exec.LookPath(bin)
		return err == nil
	}
	return false
}

// featureView is one entry of GET /components' features. PartialBytes is what a cancelled or failed download left on disk to resume from, which DELETE /components/{id} discards when the feature is not installed.
type featureView struct {
	ID            string   `json:"id"`
	Title         string   `json:"title"`
	Variant       string   `json:"variant"`
	DownloadBytes int64    `json:"download_bytes"`
	DiskBytes     int64    `json:"disk_bytes"`
	PartialBytes  int64    `json:"partial_bytes"`
	State         string   `json:"state"`
	Error         string   `json:"error"`
	Requires      []string `json:"requires"`
	NeedsRestart  bool     `json:"needs_restart"`
}

// view works out one feature's state. Input: the feature, the machine, the receipts and the failures on record. Output: the view, and false when the feature has no build for this platform, which leaves it off the list.
// A queued or running job outranks everything, a recorded failure outranks the files on disk (a feature whose smoke test failed has all its files and still does not work), and receipts outrank a feature merely being usable.
func (s *Service) view(f *feature, p Platform, recs map[string]receipt, failed map[string]string) (featureView, bool) {
	v := featureView{ID: f.ID, Title: f.Title, Requires: f.Requires, NeedsRestart: f.NeedsRestart}
	if v.Requires == nil {
		v.Requires = []string{}
	}
	variant, parts, old, installed := s.installedVariant(f, p, recs)
	queueState, jobVariant, jobParts := s.jobFor(f.ID)
	switch {
	case queueState != "" && jobParts != nil:
		// The card describes the build being installed, which after a resumed job or a fall back from CUDA is not necessarily the one "auto" would pick now, so its sizes match the progress events'.
		variant, parts = jobVariant, jobParts
	case !installed:
		var ok bool
		if variant, parts, ok = s.man.pick(f, "auto", p); !ok {
			return v, false
		}
	}
	v.Variant = variant.Name
	var all int64
	for _, pt := range parts {
		all += pt.Art.Size
		v.DiskBytes += pt.Art.diskBytes()
		if s.status(pt, recs) != partCurrent {
			v.DownloadBytes += pt.Art.Size
		}
	}
	if v.DownloadBytes == 0 {
		v.DownloadBytes = all
	}
	v.PartialBytes = s.featurePartialBytes(f)
	switch {
	case installed && old:
		v.State = "update_available"
	case installed:
		v.State = "installed"
	case !s.anyReceipt(f, recs) && s.usable(f.ID):
		v.State = "external"
	default:
		v.State = "not_installed"
	}
	if msg, ok := failed[f.ID]; ok {
		v.State, v.Error = "failed", msg
	}
	if queueState != "" {
		v.State, v.Error = queueState, ""
	}
	return v, true
}

// featurePartialBytes adds up the partial downloads of every build of every component a feature uses, since a download cancelled under one variant is still on disk after the machine's choice moves to another.
func (s *Service) featurePartialBytes(f *feature) int64 {
	seen := map[string]bool{}
	var n int64
	for _, id := range s.man.componentIDs(f) {
		for _, a := range s.man.Components[id] {
			if seen[a.SHA256] || !a.fitsHere() {
				continue
			}
			seen[a.SHA256] = true
			n += s.partialBytes(a)
		}
	}
	return n
}

// anyReceipt reports whether June installed any part of a feature that is the feature's own, in any of its variants. A feature June has begun installing is June's, even when what is there so far is enough for the daemon to use, so it is never mistaken for one put there by hand. A part another feature also uses (llama-server) says nothing about which of the two June installed, so it is not counted.
func (s *Service) anyReceipt(f *feature, recs map[string]receipt) bool {
	for _, id := range s.man.componentIDs(f) {
		if _, ok := recs[id]; ok && !s.man.shared(id, f) {
			return true
		}
	}
	return false
}

// componentExternal reports whether a component's main file is on disk with no receipt behind it, which is how one put there by hand looks.
func (s *Service) componentExternal(pt part, recs map[string]receipt) bool {
	if _, ok := recs[pt.ID]; ok {
		return false
	}
	return util.Exists(s.abs(pt.mainPath()))
}

// partialMaxAge is how long a partial download nobody has come back for is kept. The window only offers to discard one for a feature that is not installed, so without this a 2.8 GB model cancelled and forgotten would hold its space for good.
const partialMaxAge = 14 * 24 * time.Hour

// sweep finishes deletions an earlier remove or update could not do because the file was in use, and clears out what an interrupted job left: its staging directory, partial downloads of files the manifest no longer pins, and partial downloads untouched for partialMaxAge. It runs at start, before every job and before every remove, when nothing else is touching these directories. Input: the partial files, by the first eight characters of their hash, of jobs still to run, which are kept however old: a job taken up after a fortnight with the computer off resumes rather than starting over.
func (s *Service) sweep(keep map[string]bool) {
	root := filepath.Join(s.opts.DataDir, "components")
	os.RemoveAll(filepath.Join(root, ".staging"))
	pinned := map[string]bool{}
	for _, vs := range s.man.Components {
		for _, a := range vs {
			pinned[a.SHA256[:8]] = true
		}
	}
	if entries, err := os.ReadDir(filepath.Join(root, ".partial")); err == nil {
		// Age is the download's own, not its note's: the note is written when an attempt starts and the bytes keep arriving after it.
		stale := map[string]bool{}
		for _, e := range entries {
			if sha8, ok := strings.CutSuffix(e.Name(), ".part"); ok && !keep[sha8] {
				if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > partialMaxAge {
					stale[sha8] = true
				}
			}
		}
		for _, e := range entries {
			sha8, _, _ := strings.Cut(e.Name(), ".")
			if !pinned[sha8] || stale[sha8] {
				os.Remove(filepath.Join(root, ".partial", e.Name()))
			}
		}
	}
	s.sweepTrash()
}

// onDisk is how far a set of parts has got: the size of every part already installed and current, plus the bytes of every partial download. It only grows while a job makes headway, so comparing it across a restart tells a download cut short from a job that brings the daemon down before getting anywhere.
func (s *Service) onDisk(parts []part, recs map[string]receipt) int64 {
	var n int64
	for _, pt := range parts {
		if s.status(pt, recs) == partCurrent {
			n += pt.Art.Size
			continue
		}
		n += s.partialBytes(pt.Art)
	}
	return n
}

// jobPartials is the keep set sweep takes for a list of jobs: the hash prefix of every part they may download.
func jobPartials(jobs []*job) map[string]bool {
	keep := map[string]bool{}
	for _, j := range jobs {
		for _, pt := range j.parts {
			keep[pt.Art.SHA256[:8]] = true
		}
	}
	return keep
}

// sweepTrash deletes the files on the trash list that are no longer in use. sweep runs it at every start too, which is when a model llama-server had mapped has been let go of, so that space comes back without waiting for the next install.
func (s *Service) sweepTrash() {
	var gone []string
	s.editState(func(st *stateFile) {
		var left []string
		for _, rel := range st.Trash {
			if err := os.Remove(s.abs(rel)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				left = append(left, rel)
				continue
			}
			gone = append(gone, rel)
		}
		st.Trash = left
	})
	s.pruneDirs(gone)
}

// samePath compares two paths the way the file system does: case-insensitively on Windows.
func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// Status reports, for june doctor, how each local feature got onto this machine: "managed" when June installed it, "external" when its files were put there by hand, "missing" when it is not there. Input: the data directory. Output: the state by feature id; a feature with no build for this platform is left out.
func Status(dataDir string) map[string]string {
	s := newService(Options{DataDir: dataDir})
	if s.man == nil {
		return map[string]string{}
	}
	p := s.platformCached()
	recs := s.receipts()
	out := map[string]string{}
	for _, f := range s.man.Features {
		v, ok := s.view(f, p, recs, nil)
		if !ok {
			continue
		}
		switch v.State {
		case "installed", "update_available":
			out[f.ID] = "managed"
		case "external":
			out[f.ID] = "external"
		default:
			out[f.ID] = "missing"
		}
	}
	return out
}
