package components

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"testing"
)

// backgroundService is a downloader on an empty data directory, with no whisper or sherpa anywhere this machine's PATH could find, so nothing counts as already set up by hand.
func backgroundService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("JUNE_DATA_DIR", dir)
	t.Setenv("PATH", t.TempDir())
	s := newService(Options{DataDir: dir})
	if s.man == nil {
		t.Fatal("the embedded manifest did not load")
	}
	return s
}

func jobIDs(jobs []*job) []string {
	var ids []string
	for _, j := range jobs {
		ids = append(ids, j.id)
	}
	return ids
}

func TestBackgroundJobs_TakesTheManifestsBackgroundFeaturesOnly(t *testing.T) {
	s := backgroundService(t)
	var want []string
	for _, f := range s.man.Features {
		if f.Background {
			want = append(want, f.ID)
		}
	}
	if len(want) == 0 {
		t.Fatal("no feature in the manifest is marked background")
	}
	jobs := s.backgroundJobs(Platform{OS: runtime.GOOS, FreeBytes: 100 << 30, RAMMB: 16 << 10}, nil, nil)
	if got := jobIDs(jobs); !slices.Equal(got, want) {
		t.Fatalf("jobs = %v, want %v", got, want)
	}
	for _, j := range jobs {
		for _, req := range s.man.feature(j.id).Requires {
			if !slices.Contains(j.after, req) {
				t.Errorf("%s does not wait for %s, which it requires", j.id, req)
			}
		}
	}
}

func TestBackgroundJobs_LeavesWhatCannotBeStartedToThePerson(t *testing.T) {
	s := backgroundService(t)
	roomy := Platform{OS: runtime.GOOS, FreeBytes: 100 << 30, RAMMB: 16 << 10}
	cases := []struct {
		name   string
		p      Platform
		failed map[string]string
	}{
		{name: "too little disk", p: Platform{OS: runtime.GOOS, FreeBytes: 100 << 20, RAMMB: 16 << 10}},
		{name: "a failure on record is not retried unasked", p: roomy, failed: map[string]string{"transcribe": "the download did not match its checksum"}},
	}
	// Where the machine's own build is the large GPU one, it is for the person to pick.
	if runtime.GOOS == "windows" {
		gpu := roomy
		gpu.GPU = &GPU{Vendor: "nvidia", Name: "test card", VRAMMB: 8 << 10, Driver: "551.23"}
		cases = append(cases, struct {
			name   string
			p      Platform
			failed map[string]string
		}{name: "a build over the size limit", p: gpu})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := jobIDs(s.backgroundJobs(c.p, nil, c.failed)); len(got) != 0 {
				t.Fatalf("jobs = %v, want none", got)
			}
		})
	}
}

// The routes go on one mux, which panics on patterns that overlap, and POST /components/background must not be taken for a feature id.
func TestRoutes_BackgroundIsNotTakenForAFeatureID(t *testing.T) {
	s := backgroundService(t)
	mux := http.NewServeMux()
	for pattern, h := range s.Routes() {
		mux.HandleFunc(pattern, h)
	}
	_, pattern := mux.Handler(httptest.NewRequest(http.MethodPost, "/components/background", nil))
	if pattern != "POST /components/background" {
		t.Fatalf("POST /components/background is answered by %q", pattern)
	}
}
