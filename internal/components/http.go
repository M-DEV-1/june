package components

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"june/internal/lifecycle"
	"june/internal/util"
)

// Routes answers GET /components, POST /components/{id}/install, POST /components/{id}/cancel and DELETE /components/{id}, keyed by ServeMux pattern for cmd to register behind the IPC token. {id} is a feature id or, so CI can install one small file, a single component's.
func (s *Service) Routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"GET /components":               s.handleList,
		"POST /components/{id}/install": s.handleInstall,
		"POST /components/{id}/cancel":  s.handleCancel,
		"DELETE /components/{id}":       s.handleRemove,
	}
}

// writeError answers with a JSON body whose "error" is a fixed word the window branches on and whose "message" is for a person.
func writeError(w http.ResponseWriter, code int, word, message string, extra map[string]any) {
	body := map[string]any{"error": word, "message": message}
	for k, v := range extra {
		body[k] = v
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(body)
}

func (s *Service) handleList(w http.ResponseWriter, r *http.Request) {
	if s.man == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "this build of June has no local features", nil)
		return
	}
	p := s.platformCached()
	recs := s.receipts()
	s.stateMu.Lock()
	failed := s.loadState().Failed
	s.stateMu.Unlock()
	features := []featureView{}
	for _, f := range s.man.Features {
		v, ok := s.view(f, p, recs, failed)
		if !ok {
			continue
		}
		// A feature this machine has too little memory for is not offered at all, rather than shown with a button that can only refuse; one already there is still listed, so it can be removed.
		if f.MinRAMMB > 0 && p.RAMMB > 0 && p.RAMMB < f.MinRAMMB && v.State == "not_installed" {
			continue
		}
		features = append(features, v)
	}
	util.WriteJSON(w, map[string]any{"platform": p, "features": features, "restart_pending": lifecycle.RestartPending()})
}

// handleInstall queues an install and answers 202 at once; progress follows as "component" events. Refusals are newJob's, and 503 "closing" while the daemon shuts down.
func (s *Service) handleInstall(w http.ResponseWriter, r *http.Request) {
	if s.man == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "this build of June has no local features", nil)
		return
	}
	id := r.PathValue("id")
	var body struct {
		Variant string `json:"variant"`
	}
	if r.Body != nil {
		json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body)
	}
	j, ref := s.newJob(id, body.Variant, s.platformCached(), s.receipts(), nil)
	if ref != nil {
		writeError(w, ref.code, ref.word, ref.message, ref.extra)
		return
	}
	if err := s.enqueue(j); err != nil {
		writeError(w, http.StatusServiceUnavailable, "closing", err.Error(), nil)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"id": id, "state": s.jobState(id)})
}

// refusal is why an install cannot be queued: the status and the fixed word the window branches on, a sentence for a person, and any extra fields of the error body.
type refusal struct {
	code    int
	word    string
	message string
	extra   map[string]any
}

// newJob works out what installing id means on this machine, for a request from the window and for a job a restart interrupted alike. Input: a feature or component id, the variant asked for ("" or "auto" for the machine's own choice, which for a feature already installed is the build it has), the machine, the receipts, and the ids of jobs accepted ahead of this one that are not in the queue yet (resume re-queues a whole list at once). Output: the job, or why it cannot be queued: 404 "unknown" for an id that is neither a feature nor a component, 400 "unsupported" or "variant" for a build this machine has none of, 409 for a feature put there by hand ("external"), one whose prerequisite is missing ("requires") or one this machine has too little memory for ("low_memory"), and 507 "disk_full" when the disk cannot hold it.
func (s *Service) newJob(id, variant string, p Platform, recs map[string]receipt, coming map[string]bool) (*job, *refusal) {
	j := &job{id: id}
	if f := s.man.feature(id); f != nil {
		view, ok := s.view(f, p, recs, nil)
		if !ok {
			return nil, &refusal{http.StatusBadRequest, "unsupported", f.Title + " has no build for this computer", nil}
		}
		if view.State == "external" {
			return nil, &refusal{http.StatusConflict, "external", f.Title + " was set up by hand, so June leaves it alone", nil}
		}
		if f.MinRAMMB > 0 && p.RAMMB > 0 && p.RAMMB < f.MinRAMMB {
			return nil, &refusal{http.StatusConflict, "low_memory", f.Title + " needs 8 GB of memory; this computer has " + humanBytes(int64(p.RAMMB)<<20), nil}
		}
		for _, req := range f.Requires {
			switch {
			case coming[req] || s.jobState(req) != "":
				// Still on its way, so the job checks again when its turn comes, and fails then rather than install something its prerequisite's failure left useless.
				j.after = append(j.after, req)
			case !s.satisfied(req, p, recs):
				return nil, &refusal{http.StatusConflict, "requires", s.man.requiresMessage(f, req), map[string]any{"requires": f.Requires}}
			}
		}
		name := variant
		installed, oldParts, _, isInstalled := s.installedVariant(f, p, recs)
		if isInstalled && (name == "" || name == "auto") && p.meets(installed.Needs) {
			// A repair or an update keeps the build the machine already has, rather than switching under the user. One the machine no longer meets the needs of — a CUDA build that failed its test on this card — is switched away from, which is how a cancelled switch to the CPU build is finished.
			name = installed.Name
		}
		v, parts, ok := s.man.pick(f, name, p)
		if !ok {
			return nil, &refusal{http.StatusBadRequest, "variant", f.Title + " has no " + variant + " build for this computer", nil}
		}
		j.feat, j.variant, j.parts = f, v, parts
		if isInstalled && v.Name != installed.Name {
			j.replaces, j.replacesName = oldParts, installed.Name
		}
		// The feature's own components June installed for another build and this one does not use — the medium model a switch to the CPU build cut short left behind — go once this build works, rather than holding gigabytes nothing reads.
		j.replaces = append(j.replaces, s.leftovers(f, parts, recs)...)
	} else if _, ok := s.man.Components[id]; ok {
		ref := id
		if variant != "" && variant != "auto" {
			ref += "@" + variant
		}
		pt, ok := s.man.resolve(ref, p)
		if !ok {
			return nil, &refusal{http.StatusBadRequest, "variant", id + " has no such build for this computer", nil}
		}
		if s.componentExternal(pt, recs) {
			return nil, &refusal{http.StatusConflict, "external", pt.Art.Main + " was put there by hand, so June leaves it alone", nil}
		}
		j.variant, j.parts = featureVariant{Name: pt.Variant}, []part{pt}
	} else {
		return nil, &refusal{http.StatusNotFound, "unknown", "no local feature or component is called " + id, nil}
	}
	if need := s.needBytes(j.parts, recs); p.FreeBytes > 0 && p.FreeBytes < need {
		return nil, &refusal{http.StatusInsufficientStorage, "disk_full", "not enough disk space: " + humanBytes(need) + " needed, " + humanBytes(p.FreeBytes) + " free", map[string]any{"need_bytes": need, "free_bytes": p.FreeBytes}}
	}
	j.mark = s.onDisk(j.parts, recs)
	return j, nil
}

// satisfied reports whether a feature another one requires is there, or on its way in the queue.
func (s *Service) satisfied(id string, p Platform, recs map[string]receipt) bool {
	f := s.man.feature(id)
	if f == nil {
		return true
	}
	v, ok := s.view(f, p, recs, nil)
	if !ok {
		return false
	}
	switch v.State {
	case "installed", "update_available", "external", "queued", "installing":
		return true
	}
	return false
}

// dependents lists the features June installed that require f, which removing f would leave unable to run. One put there by hand is not counted: June cannot remove it, so counting it would leave f impossible to remove too. Output: their ids and titles.
func (s *Service) dependents(f *feature, p Platform, recs map[string]receipt) ([]string, []string) {
	var ids, titles []string
	for _, g := range s.man.Features {
		for _, req := range g.Requires {
			if req != f.ID {
				continue
			}
			if v, ok := s.view(g, p, recs, nil); ok && (v.State == "installed" || v.State == "update_available") {
				ids, titles = append(ids, g.ID), append(titles, g.Title)
			}
		}
	}
	return ids, titles
}

// handleCancel stops a queued or running install. It answers 204 whether or not there was one, since a download that finished a moment before the click needs no stopping.
func (s *Service) handleCancel(w http.ResponseWriter, r *http.Request) {
	s.cancelJob(r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

// handleRemove deletes what June installed for a feature or component, or for one not installed the download it left to resume from, and answers whether June now needs a restart to stop using it. Refusals: 404 for an unknown id, 409 while an install is queued or running or another remove is under way ("busy"), for files put there by hand ("external"), or for a feature another one June installed needs ("required_by", with their ids).
// The remove holds the component files for its whole length, so an install asked for meanwhile waits in the queue instead of finding llama-server current just before it is deleted, or having its staging directory swept away while it unpacks.
func (s *Service) handleRemove(w http.ResponseWriter, r *http.Request) {
	if s.man == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "this build of June has no local features", nil)
		return
	}
	id := r.PathValue("id")
	if !s.beginExclusive() {
		writeError(w, http.StatusConflict, "busy", "wait for the download in progress to finish, or cancel it", nil)
		return
	}
	defer s.endExclusive()
	p := s.platformCached()
	recs := s.receipts()
	var err error
	if f := s.man.feature(id); f != nil {
		if v, ok := s.view(f, p, recs, nil); ok && v.State == "external" {
			writeError(w, http.StatusConflict, "external", f.Title+" was set up by hand, so June will not delete it", nil)
			return
		}
		// Who said what only ever runs inside a transcription, so taking transcription away under it would leave it shown as set up while it can never run.
		if ids, titles := s.dependents(f, p, recs); len(ids) > 0 {
			verb := " first; it needs "
			if len(ids) > 1 {
				verb = " first; they need "
			}
			writeError(w, http.StatusConflict, "required_by", "Remove "+strings.Join(titles, " and ")+verb+f.Title, map[string]any{"required_by": ids})
			return
		}
		err = s.remove(id, f, s.man.componentIDs(f))
	} else if _, ok := s.man.Components[id]; ok {
		if pt, ok := s.man.resolve(id, p); ok && s.componentExternal(pt, recs) {
			writeError(w, http.StatusConflict, "external", pt.Art.Main+" was put there by hand, so June will not delete it", nil)
			return
		}
		err = s.remove(id, nil, []string{id})
	} else {
		writeError(w, http.StatusNotFound, "unknown", "no local feature or component is called "+id, nil)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "config", err.Error(), nil)
		return
	}
	util.WriteJSON(w, map[string]bool{"restart_pending": lifecycle.RestartPending()})
}
