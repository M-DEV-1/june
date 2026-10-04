package components

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"time"

	"june/internal/config"
	"june/internal/lifecycle"
)

// diskMargin is room left over after an install, so a download never takes the last of the disk the store and the recordings write to.
const diskMargin = 500 << 20

// errInUse is a file June must replace or delete while a program still holds it open. A model llama-server has mapped cannot even be renamed on Windows.
var errInUse = errors.New("in use — finish the meeting or dictation first")

// needBytes is the free space a set of parts needs: what is still to be downloaded, what the archives unpack to beside their download, and diskMargin. Parts already installed and current cost nothing.
func (s *Service) needBytes(parts []part, recs map[string]receipt) int64 {
	need := int64(diskMargin)
	for _, pt := range parts {
		if s.status(pt, recs) == partCurrent {
			continue
		}
		need += pt.Art.Size - s.partialBytes(pt.Art)
		if pt.Art.Format != "file" {
			need += pt.Art.diskBytes()
		}
	}
	return need
}

// run carries one job through: every part not already installed is fetched, unpacked and moved into place in turn, with its receipt written as soon as its files are in; then the feature is smoke-tested and the config pointed at it. A cancel stops the job between steps, or mid-download with the bytes kept for next time; once files start moving into place the job finishes regardless, so a cancel never leaves a component half-replaced. Input: the job, and the partials of every queued job, which the sweep before it keeps. Output: false when the daemon stopping interrupted the job, which then stays queued for the next start; true when it ended one way or another.
func (s *Service) run(j *job, keep map[string]bool) bool {
	if msg, ok := s.prerequisiteFailed(j); ok {
		s.recordFailure(j.id, msg)
		return true
	}
	s.sweep(keep)
	err := s.install(j, j.parts)
	if err == nil && j.feat != nil {
		err = s.finishFeature(j)
	} else if err == nil {
		err = s.smokePart(s.life, j.id, j.parts[0])
	}
	if err != nil && s.life.Err() != nil && !s.cancelledByUser(j) {
		slog.Info("component install interrupted by the daemon stopping; the next start takes it up", "id", j.id, "error", err)
		return false
	}
	switch {
	case err == nil:
		s.editState(func(st *stateFile) { delete(st.Failed, j.id) })
		s.publish(j.id, "installed", map[string]any{"variant": j.variant.Name, "restart_pending": lifecycle.RestartPending()}, false)
		slog.Info("component installed", "id", j.id, "variant", j.variant.Name)
	case errors.Is(err, context.Canceled):
		s.publish(j.id, "cancelled", nil, false)
		slog.Info("component install cancelled", "id", j.id)
	default:
		if errors.Is(err, errCUDABlocked) {
			// The CUDA build cannot be used here while Smart App Control refuses its ggml-cuda.dll, so the card's Try again sets up the CPU build, which the message offers, rather than the same blocked build again.
			s.markNoCUDA(true)
		}
		s.recordFailure(j.id, err.Error())
	}
	return true
}

// prerequisiteFailed checks again, when a job's turn comes, the prerequisites that were still queued when it was asked for. One that failed, or was cancelled, leaves this feature with nothing to run inside, so the job fails at once rather than download it. Output: the reason, and true when the job must not run.
func (s *Service) prerequisiteFailed(j *job) (string, bool) {
	if len(j.after) == 0 || j.feat == nil {
		return "", false
	}
	failed := s.loadStateLocked().Failed
	p, recs := s.platformCached(), s.receipts()
	for _, req := range j.after {
		if _, bad := failed[req]; bad || !s.satisfied(req, p, recs) {
			return s.man.requiresMessage(j.feat, req), true
		}
	}
	return "", false
}

// install puts every part in place that is not already installed and current. Input: the job, for its context and progress, and the parts. Output: the first error.
func (s *Service) install(j *job, parts []part) error {
	recs := s.receipts()
	var todo []part
	var total int64
	for _, pt := range parts {
		if s.status(pt, recs) == partCurrent {
			continue
		}
		todo = append(todo, pt)
		total += pt.Art.Size
	}
	if free := freeBytes(s.opts.DataDir); free > 0 {
		if need := s.needBytes(todo, recs); free < need {
			return fmt.Errorf("not enough disk space: %s needed, %s free", humanBytes(need), humanBytes(free))
		}
	}
	prog := newProgress(s, j.id, total)
	sac := smartAppControl() == "on"
	for i, pt := range todo {
		if err := s.installPart(j.ctx, pt, prog); err != nil {
			return err
		}
		if sac && i < len(todo)-1 {
			if err := s.tryStart(j, pt); err != nil {
				return err
			}
		}
	}
	return nil
}

// tryStart runs a program part on its own, for its help or version, as soon as it is in place and before the models after it are fetched, on a PC where Smart App Control is on. Smart App Control judges an unsigned program by Microsoft's reputation for it, which cannot be read beforehand, and a block that only the feature's test at the end found would come after gigabytes of models, or after the CUDA build's half gigabyte and then the CPU build's. A block stops the job, and so does any program that never ran (startError, a DLL missing or the wrong one): the feature's test at the end would fail the same way, and the CPU build is never fetched for it. A program that ran and exited with an error is left to that test, which runs it the way the daemon does. Input: the job and the part just installed. Output: the block or the start failure, or the job's cancel.
func (s *Service) tryStart(j *job, pt part) error {
	err := s.smokePart(j.ctx, j.id, pt)
	var blocked appControlError
	var notRun startError
	switch {
	case j.ctx.Err() != nil:
		return j.ctx.Err()
	case errors.As(err, &blocked), errors.As(err, &notRun):
		return err
	case err != nil:
		slog.Info("a component did not pass its early start check; the feature's own test decides", "id", pt.ID, "error", err)
	}
	return nil
}

// installPart fetches one part, unpacks it into staging, adds the C runtime when the build needs it, moves its files into place and writes its receipt. The download is deleted once its files are in; a cancelled or failed one is kept for the next attempt to resume.
func (s *Service) installPart(ctx context.Context, pt part, prog *progress) error {
	a := pt.Art
	file, err := s.fetch(ctx, a, prog)
	if err != nil {
		return err
	}
	var files []string
	if a.Format == "file" {
		if err := ctx.Err(); err != nil {
			return err
		}
		// The download itself is the file, renamed into place on the same volume, so a 2.8 GB model is never copied, and is still there to retry with if the rename cannot happen.
		files, err = s.place(map[string]string{a.Main: file}, a.Dest)
	} else {
		files, err = s.unpack(ctx, pt, file, prog)
	}
	if err != nil {
		if len(files) > 0 {
			// Some files moved before the failure. A receipt with no hash keeps them June's, so the next Set up replaces them instead of treating them as put there by hand.
			incomplete := pt
			incomplete.Art = &artifact{SHA256: ""}
			s.recordReceipt(incomplete, files)
		}
		return err
	}
	s.recordReceipt(pt, files)
	os.Remove(file)
	os.Remove(file + ".json")
	return nil
}

// unpack extracts an archive part into its staging directory, adds the C runtime when the build needs it and moves the result into place. Output: the files placed, as place gives them.
func (s *Service) unpack(ctx context.Context, pt part, archive string, prog *progress) ([]string, error) {
	a := pt.Art
	staging := filepath.Join(s.opts.DataDir, "components", ".staging", pt.ID)
	os.RemoveAll(staging)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return nil, err
	}
	defer os.RemoveAll(staging)
	s.publishStep(prog, "extracting", a.fileName())
	if err := extract(ctx, archive, staging, a); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if diskFull(err) {
			return nil, fmt.Errorf("not enough disk space to unpack %s; free some space and try again", a.fileName())
		}
		return nil, fmt.Errorf("unpacking %s: %w", a.fileName(), err)
	}
	if !exists(filepath.Join(staging, filepath.FromSlash(a.Main))) {
		return nil, fmt.Errorf("%s does not contain %s", a.fileName(), a.Main)
	}
	if a.CRT && runtime.GOOS == "windows" {
		if err := s.copyCRT(staging); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	srcs := map[string]string{}
	err := filepath.WalkDir(staging, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(staging, p)
		if err != nil {
			return err
		}
		srcs[filepath.ToSlash(rel)] = p
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.place(srcs, a.Dest)
}

// crtDLLs are the Visual C++ runtime DLLs the installer ships in <exe dir>/crt. whisper.cpp's and llama.cpp's Windows builds import msvcp140, vcruntime140 and vcruntime140_1, and ggml-base.dll, which whisper-cli imports through whisper.dll and so needs before its first line, imports OpenMP's vcomp140 as well, as do the ggml-cpu-*.dll backends; none of them is part of Windows.
var crtDLLs = []string{"msvcp140.dll", "vcruntime140.dll", "vcruntime140_1.dll", "vcomp140.dll"}

// copyCRT puts the Visual C++ runtime DLLs the installer shipped in <exe dir>/crt beside a build that needs them, since a PC without the VC++ redistributable lacks them and installing that needs admin. A missing crt directory is not an error: a developer build has none, and a PC with the redistributable needs none; the smoke test says so if the build cannot start. A crt directory missing one of crtDLLs is an installer that shipped short, which is logged, since the smoke test's "a DLL it needs is missing" cannot name the file.
func (s *Service) copyCRT(staging string) error {
	if s.opts.ExeDir == "" {
		return nil
	}
	dir := filepath.Join(s.opts.ExeDir, "crt")
	if !exists(dir) {
		return nil
	}
	var missing []string
	for _, name := range crtDLLs {
		src := filepath.Join(dir, name)
		if !exists(src) {
			missing = append(missing, name)
			continue
		}
		dst := filepath.Join(staging, name)
		if exists(dst) {
			continue
		}
		if err := copyFile(src, dst); err != nil {
			return fmt.Errorf("copying the C runtime: %w", err)
		}
	}
	if len(missing) > 0 {
		slog.Warn("June's crt folder lacks part of the C runtime; a downloaded build may not start on a PC without the Visual C++ redistributable", "dir", dir, "missing", missing)
	}
	return nil
}

// place moves files into <data>/<dest>, each to its relative path there. Every target is cleared before any file moves, so a file that is in use stops the move before the old build and the new one are mixed. A file already there is deleted, or when a running program holds it (an exe or DLL whisper-cli or llama-server has loaded) renamed aside and deleted by a later sweep; one that cannot even be renamed fails the move with errInUse. Input: the sources by relative path (forward slashes), and dest relative to the data directory. Output: the files placed, relative to the data directory with forward slashes.
func (s *Service) place(srcs map[string]string, dest string) ([]string, error) {
	rels := make([]string, 0, len(srcs))
	for rel := range srcs {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	target := func(rel string) string {
		return filepath.Join(s.opts.DataDir, filepath.FromSlash(dest), filepath.FromSlash(rel))
	}
	for _, rel := range rels {
		if err := os.MkdirAll(filepath.Dir(target(rel)), 0o755); err != nil {
			return nil, err
		}
		if err := s.clear(target(rel)); err != nil {
			return nil, fmt.Errorf("%s: %w", path.Base(rel), err)
		}
	}
	var placed []string
	for _, rel := range rels {
		if err := renameRetry(srcs[rel], target(rel)); err != nil {
			slog.Warn("could not move a component file into place", "file", target(rel), "error", err)
			return placed, fmt.Errorf("could not move %s into place; try again", path.Base(rel))
		}
		placed = append(placed, dest+"/"+rel)
	}
	return placed, nil
}

// renameRetry renames a file it has only just written, retrying for a couple of seconds: on Windows an antivirus scanner opens every new executable and DLL the moment it appears, and a rename during that scan fails with a sharing violation that clears by itself.
func renameRetry(src, dst string) error {
	var err error
	for i := 0; i < 10; i++ {
		if err = os.Rename(src, dst); err == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return err
}

// clear makes way for a file at target: deletes what is there, or renames it aside onto the trash list when it is in use. Output: errInUse when it can be neither deleted nor renamed.
func (s *Service) clear(target string) error {
	if _, err := os.Lstat(target); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err := os.Remove(target); err == nil {
		return nil
	}
	aside := target + ".june-old-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := os.Rename(target, aside); err != nil {
		return errInUse
	}
	rel, err := filepath.Rel(s.opts.DataDir, aside)
	if err != nil {
		return nil
	}
	s.editState(func(st *stateFile) { st.Trash = append(st.Trash, filepath.ToSlash(rel)) })
	return nil
}

// recordReceipt writes a part's receipt, replacing the component's earlier one. Files the earlier build had and this one does not — ggml-cuda.dll and the CUDA libraries when the CPU build replaces the CUDA one — are deleted, so switching builds leaves nothing of the old one behind.
func (s *Service) recordReceipt(pt part, files []string) {
	keep := map[string]bool{}
	for _, f := range files {
		keep[f] = true
	}
	var stale []string
	s.editState(func(st *stateFile) {
		var out []receipt
		for _, r := range st.Components {
			if r.ID == pt.ID {
				for _, f := range r.Files {
					if !keep[f] {
						stale = append(stale, f)
					}
				}
				continue
			}
			out = append(out, r)
		}
		st.Components = append(out, receipt{ID: pt.ID, Variant: pt.Variant, SHA256: pt.Art.SHA256, Files: files, InstalledAt: time.Now().UTC()})
	})
	s.deleteFiles(stale)
}

// deleteFiles removes files June installed, putting any that are in use on the trash list, then removes the directories that leaves empty.
func (s *Service) deleteFiles(rels []string) {
	for _, rel := range rels {
		if err := s.clear(s.abs(rel)); err != nil {
			s.editState(func(st *stateFile) { st.Trash = append(st.Trash, rel) })
		}
	}
	s.pruneDirs(rels)
}

// pruneDirs removes every directory above the given files, up to the data directory, that is now empty. A directory with anything left in it, such as a model put there by hand, stays.
func (s *Service) pruneDirs(rels []string) {
	dirs := map[string]bool{}
	for _, rel := range rels {
		for d := filepath.Dir(s.abs(rel)); len(d) > len(s.opts.DataDir); d = filepath.Dir(d) {
			dirs[d] = true
		}
	}
	// Deepest first, so a directory is tried only after everything under it.
	var list []string
	for d := range dirs {
		list = append(list, d)
	}
	sort.Slice(list, func(i, k int) bool { return len(list[i]) > len(list[k]) })
	for _, d := range list {
		os.Remove(d)
	}
}

// finishFeature smoke-tests a feature once its parts are in and points the config at it. A CUDA whisper build that cannot decode on the card is swapped for the CPU variant, model and all, rather than failing: the card or its driver is not what the detection thought, and the CPU build is what such a machine should have had. One kept off the card by Smart App Control (errCUDABlocked) fails instead, so a build that may be blocked as well is not fetched unasked.
func (s *Service) finishFeature(j *job) error {
	// The test runs to the end whatever the window asks: by now every file is in place, and a feature left installed but never tested or configured would read as installed while doing nothing. Only the daemon stopping ends it, and then the next start runs it again.
	whole := s.life
	err := s.smokeFeature(whole, j.id, j.feat.ID, j.parts)
	if err != nil && errors.Is(err, errNoCUDA) {
		slog.Warn("the CUDA whisper build did not run on the GPU, installing the CPU build instead", "error", err)
		s.markNoCUDA(true)
		p := s.platformCached()
		v, parts, ok := s.man.pick(j.feat, "cpu", p)
		if !ok {
			return err
		}
		// The queue on disk now says CPU, replacing CUDA, so a restart part-way through the CPU download carries on with it and still clears the CUDA build's leftovers at the end, rather than fetching the CUDA build again.
		cuda := j.parts
		mark := s.onDisk(parts, s.receipts())
		s.mu.Lock()
		j.replaces, j.replacesName = append(j.replaces, cuda...), j.variant.Name
		j.variant, j.parts, j.mark = v, parts, mark
		s.savePendingLocked()
		s.mu.Unlock()
		if err := s.install(j, parts); err != nil {
			if errors.Is(err, context.Canceled) && s.life.Err() == nil {
				// A cancel here would otherwise read as a clean stop and leave the CUDA build that just failed its test showing as installed, with medium running on the CPU and nothing to say so.
				return errSwitchCancelled
			}
			return err
		}
		s.dropUnused(cuda, parts)
		err = s.smokeFeature(whole, j.id, j.feat.ID, parts)
	}
	if err != nil {
		return err
	}
	if j.variant.Needs == "cuda" {
		// A CUDA build asked for by name passed on this card after all, so "auto" may choose it again.
		s.markNoCUDA(false)
	}
	if len(j.replaces) > 0 {
		s.dropUnused(j.replaces, j.parts)
	}
	return s.configure(j.feat, j.parts, true)
}

// errSwitchCancelled is a cancel during the download of the CPU build that replaces a CUDA build which could not use the card. The next Set up finishes the switch, since the card stays marked as one the CUDA build does not run on; the card shows this as a failure, whose button reads Try again.
var errSwitchCancelled = errors.New("the GPU build could not use this graphics card, and switching to the CPU build was cancelled; choose Try again to finish switching")

// markNoCUDA records, or clears, that the CUDA whisper build failed its test on this machine's card and driver, so a later Set up or repair picks the CPU build instead of fetching 2 GB again only to fall back. Input: true to record, false to clear.
func (s *Service) markNoCUDA(on bool) {
	g := s.platformCached().GPU
	s.editState(func(st *stateFile) {
		switch {
		case !on:
			st.NoCUDA = nil
		case g != nil:
			st.NoCUDA = &cudaMark{GPU: g.Name, Driver: g.Driver}
		}
	})
}

// dropUnused deletes the parts of an abandoned variant that the new one does not use, such as whisper-medium once the CPU variant's small model has replaced it.
func (s *Service) dropUnused(old, now []part) {
	used := map[string]bool{}
	for _, pt := range now {
		used[pt.ID] = true
	}
	var drop []string
	for _, pt := range old {
		if !used[pt.ID] {
			drop = append(drop, pt.ID)
		}
	}
	s.removeComponents(drop)
}

// leftovers lists the feature's own components June installed that the given parts do not use, which is what an earlier build of the feature left behind. A component another feature also uses is never one. Only the ids of what it returns are read, so a build the manifest no longer lists comes back with no artifact.
func (s *Service) leftovers(f *feature, parts []part, recs map[string]receipt) []part {
	used := map[string]bool{}
	for _, pt := range parts {
		used[pt.ID] = true
	}
	var out []part
	for _, id := range s.man.componentIDs(f) {
		r, ok := recs[id]
		if !ok || used[id] || s.man.shared(id, f) {
			continue
		}
		out = append(out, part{ID: id, Variant: r.Variant, Art: s.man.Components[id][r.Variant]})
	}
	return out
}

// removeComponents deletes the files of every listed component June installed, and their receipts.
func (s *Service) removeComponents(ids []string) {
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var files []string
	s.editState(func(st *stateFile) {
		var keep []receipt
		for _, r := range st.Components {
			if want[r.ID] {
				files = append(files, r.Files...)
				continue
			}
			keep = append(keep, r)
		}
		st.Components = keep
	})
	s.deleteFiles(files)
}

// configure points the config at a feature June has just installed, or takes it off one being removed, and marks a restart pending for a feature the daemon only reads at start. Removing only clears settings that point at June's own files, so a path the user set by hand is left alone. Input: the feature, its parts, and whether it is being installed. Output: the config write's error.
func (s *Service) configure(f *feature, parts []part, on bool) error {
	if s.opts.UpdateConfig == nil {
		return nil
	}
	mainOf := func(id string) string {
		for _, pt := range parts {
			if pt.ID == id {
				return s.abs(pt.mainPath())
			}
		}
		return ""
	}
	ours := func(p string) bool {
		if p == "" {
			return false
		}
		for _, pt := range parts {
			if samePath(p, s.abs(pt.mainPath())) {
				return true
			}
		}
		return false
	}
	changed := false
	err := s.opts.UpdateConfig(func(c *config.JuneConfig) {
		switch f.ID {
		case "transcribe":
			for _, pt := range parts {
				if pt.ID == "whisper-medium" || pt.ID == "whisper-small" {
					switch {
					case on:
						c.Transcribe.Model = pt.Art.Main
					case c.Transcribe.Model == pt.Art.Main:
						c.Transcribe.Model = ""
					}
				}
			}
		case "memory":
			if on {
				c.Embed.LlamaServer, c.Embed.ModelPath = mainOf("llama-server"), mainOf("embeddinggemma")
				changed = true
			} else if ours(c.Embed.ModelPath) {
				c.Embed.LlamaServer, c.Embed.ModelPath = "", ""
				changed = true
			}
		case "summaries":
			if on {
				c.LocalText.LlamaServer, c.LocalText.ModelPath = mainOf("llama-server"), mainOf("gemma-e2b")
				changed = true
			} else if ours(c.LocalText.ModelPath) {
				c.LocalText.LlamaServer, c.LocalText.ModelPath = "", ""
				changed = true
			}
		}
	})
	if err != nil {
		return fmt.Errorf("saving the config: %w", err)
	}
	if changed && f.NeedsRestart {
		lifecycle.SetRestartPending(true)
	}
	return nil
}

// remove deletes what June installed for a feature or a single component: its files, its receipts, any partial download, its recorded failure and its settings in the config. A component another installed feature still uses, or that the config still names, is kept. Input: the id asked for, the feature (nil for a single component) and the component ids to consider. Output: the config write's error.
func (s *Service) remove(id string, f *feature, ids []string) error {
	s.sweep(nil)
	p := s.platformCached()
	recs := s.receipts()
	cfg, _ := config.ReadConfig()
	inUse := map[string]bool{}
	for _, g := range s.man.Features {
		if g == f {
			continue
		}
		if _, parts, _, ok := s.installedVariant(g, p, recs); ok {
			for _, pt := range parts {
				inUse[pt.ID] = true
			}
		}
	}
	// What the config still points at, apart from the settings this remove is about to clear.
	var named []string
	if f == nil || f.ID != "memory" {
		named = append(named, cfg.Embed.LlamaServer, cfg.Embed.ModelPath)
	}
	if f == nil || f.ID != "summaries" {
		named = append(named, cfg.LocalText.LlamaServer, cfg.LocalText.ModelPath)
	}
	named = append(named, cfg.Dream.ModelPath)
	var drop []string
	var parts []part
	for _, cid := range ids {
		r, ok := recs[cid]
		if ok {
			if a := s.man.Components[cid][r.Variant]; a != nil {
				parts = append(parts, part{ID: cid, Variant: r.Variant, Art: a})
			}
		}
		// Every partial goes, a shared component's too: a remove only runs with no job queued, so nothing is resuming from it, and keeping llama-server's for a feature that may never be set up would hold space the window has no other way to give back.
		for _, a := range s.man.Components[cid] {
			os.Remove(s.partialPath(a))
			os.Remove(s.partialPath(a) + ".json")
		}
		if !ok || inUse[cid] || s.named(r, named) {
			continue
		}
		drop = append(drop, cid)
	}
	var err error
	if f != nil {
		err = s.configure(f, parts, false)
	}
	s.removeComponents(drop)
	s.editState(func(st *stateFile) { delete(st.Failed, id) })
	return err
}

// named reports whether any of a receipt's files is one of the paths given.
func (s *Service) named(r receipt, paths []string) bool {
	for _, f := range r.Files {
		for _, p := range paths {
			if p != "" && samePath(p, s.abs(f)) {
				return true
			}
		}
	}
	return false
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// humanBytes writes a byte count the way the window does, for an error a person reads.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d KB", n>>10)
}
