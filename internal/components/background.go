package components

import "log/slog"

// backgroundMaxBytes is the most a feature may download for June to start it unasked. It keeps a build with a GPU runtime, such as the CUDA build of voice typing at over two gigabytes, for the person to choose on the features screen.
const backgroundMaxBytes = 1 << 30

// startBackground queues the downloads of the features the manifest marks as background ones, for first-run setup to call once the person is past the screens where a restart can happen. Nothing is queued while the machine is on battery. A feature is left for the person to start themselves when it is already installed, queued, failed or set up by hand, when its build is over backgroundMaxBytes, or when it cannot be queued, for lack of disk space say. Output: the ids queued, in manifest order.
func (s *Service) startBackground() []string {
	if s.man == nil {
		return nil
	}
	if !onMains() {
		slog.Info("not downloading local features in the background: the machine is on battery")
		return nil
	}
	s.stateMu.Lock()
	failed := s.loadState().Failed
	s.stateMu.Unlock()
	var queued []string
	for _, j := range s.backgroundJobs(s.platformCached(), s.receipts(), failed) {
		if err := s.enqueue(j); err != nil {
			break
		}
		queued = append(queued, j.id)
	}
	return queued
}

// backgroundJobs works out which background features to download now. Input: the machine, the receipts and the failures on record. Output: their jobs, in the order they go in the queue.
func (s *Service) backgroundJobs(p Platform, recs map[string]receipt, failed map[string]string) []*job {
	var jobs []*job
	coming := map[string]bool{}
	for _, f := range s.man.Features {
		v, ok := s.view(f, p, recs, failed)
		if !ok || !v.Background || v.State != "not_installed" {
			continue
		}
		j, ref := s.newJob(f.ID, "", p, recs, coming)
		if ref != nil {
			slog.Info("not downloading a local feature in the background", "id", f.ID, "reason", ref.message)
			continue
		}
		coming[f.ID] = true
		jobs = append(jobs, j)
	}
	return jobs
}
