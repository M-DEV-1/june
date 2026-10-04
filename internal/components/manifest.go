package components

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"runtime"
	"sort"
	"strings"
)

// manifestJSON is every download June can make, pinned by release, size and SHA256. It is compiled in and never fetched: a manifest read from the network would let whoever answered for that URL choose what June runs, and pinning it per June release means a release that worked keeps working when an upstream project moves on.
//
//go:embed manifest.json
var manifestJSON []byte

// artifact is one downloadable build of a component for one platform. Format "file" is the download itself, saved as Main in Dest; the archive formats are unpacked, with Strip cut off the front of every entry, only the entries matching one of Include kept, and Rename applied (a key ending in "/" renames that directory prefix). Dest is relative to the data directory and Main relative to Dest; Main is the file whose presence says the component is there, and what the smoke test runs. Needs names what the machine must have for this build to be the right one ("cuda", "vulkan"), CRT says the build needs the Visual C++ runtime the installer ships in <exe dir>/crt, and Disk is what the kept files take once unpacked.
type artifact struct {
	OS      string            `json:"os"`
	Arch    string            `json:"arch"`
	Needs   string            `json:"needs"`
	URL     string            `json:"url"`
	Size    int64             `json:"size"`
	SHA256  string            `json:"sha256"`
	Disk    int64             `json:"disk"`
	Format  string            `json:"format"`
	Strip   string            `json:"strip"`
	Include []string          `json:"include"`
	Rename  map[string]string `json:"rename"`
	Dest    string            `json:"dest"`
	Main    string            `json:"main"`
	CRT     bool              `json:"crt"`
}

// diskBytes is what the artifact takes once installed. A single file takes its own size; an archive's figure is the manifest's.
func (a *artifact) diskBytes() int64 {
	if a.Format == "file" || a.Disk == 0 {
		return a.Size
	}
	return a.Disk
}

// fileName is the name the download is shown under in progress events: the last element of its URL, unescaped, so "wespeaker_en_voxceleb_CAM%2B%2B.onnx" reads as the file it is.
func (a *artifact) fileName() string {
	u, err := url.Parse(a.URL)
	if err != nil {
		return a.Main
	}
	return path.Base(u.Path)
}

// fitsHere reports whether the artifact is built for the running OS and architecture.
func (a *artifact) fitsHere() bool {
	return (a.OS == "" || a.OS == runtime.GOOS) && (a.Arch == "" || a.Arch == runtime.GOARCH)
}

// featureVariant is one way of providing a feature on one OS: the components it needs, each written "id" or "id@variant". Variants are listed best first, and the first whose Needs the machine meets is the one "auto" picks.
type featureVariant struct {
	Name       string   `json:"name"`
	OS         string   `json:"os"`
	Needs      string   `json:"needs"`
	Components []string `json:"components"`
}

// feature is what the window offers: a thing June can do once some components are installed.
type feature struct {
	ID           string           `json:"id"`
	Title        string           `json:"title"`
	Requires     []string         `json:"requires"`
	NeedsRestart bool             `json:"needs_restart"`
	MinRAMMB     int              `json:"min_ram_mb"`
	Variants     []featureVariant `json:"variants"`
}

// manifest is manifest.json decoded: components by id, each with its builds by variant name, and the features in the order the window shows them.
type manifest struct {
	Components map[string]map[string]*artifact `json:"components"`
	Features   []*feature                      `json:"features"`
}

// part is one component pinned to one build, as a job installs it.
type part struct {
	ID      string
	Variant string
	Art     *artifact
}

// loadManifest decodes the embedded manifest and checks that every feature names components that exist. Output: the manifest, or an error naming what is wrong with it, which only a bad edit to manifest.json can cause.
func loadManifest() (*manifest, error) {
	var m manifest
	if err := json.Unmarshal(manifestJSON, &m); err != nil {
		return nil, fmt.Errorf("components manifest: %w", err)
	}
	for id, vs := range m.Components {
		for name, a := range vs {
			if len(a.SHA256) != 64 || a.Size <= 0 || a.Dest == "" || a.Main == "" || a.URL == "" {
				return nil, fmt.Errorf("components manifest: %s@%s needs a url, a size, a 64-character sha256, a dest and a main", id, name)
			}
		}
	}
	for _, f := range m.Features {
		for _, req := range f.Requires {
			if m.feature(req) == nil {
				return nil, fmt.Errorf("components manifest: feature %s requires unknown feature %s", f.ID, req)
			}
		}
		for _, v := range f.Variants {
			for _, ref := range v.Components {
				id, variant, _ := strings.Cut(ref, "@")
				vs, ok := m.Components[id]
				if !ok {
					return nil, fmt.Errorf("components manifest: feature %s names unknown component %s", f.ID, id)
				}
				if variant != "" && vs[variant] == nil {
					return nil, fmt.Errorf("components manifest: feature %s names unknown build %s", f.ID, ref)
				}
			}
		}
	}
	return &m, nil
}

// feature returns the feature with this id, or nil.
func (m *manifest) feature(id string) *feature {
	for _, f := range m.Features {
		if f.ID == id {
			return f
		}
	}
	return nil
}

// requiresMessage is the sentence for a feature refused, or failed, for want of a feature it requires.
func (m *manifest) requiresMessage(f *feature, req string) string {
	title := req
	if r := m.feature(req); r != nil {
		title = r.Title
	}
	return f.Title + " needs " + title + " set up first"
}

// resolve turns "id" or "id@variant" into the build to install here. A bare id picks the build for this platform whose needs the machine meets, preferring one that needs something over one that needs nothing, since a build that needs a GPU is only listed because it is the better one. Output: the part, or false when the component has no build for this platform.
func (m *manifest) resolve(ref string, p Platform) (part, bool) {
	id, variant, _ := strings.Cut(ref, "@")
	vs := m.Components[id]
	if variant != "" {
		a := vs[variant]
		if a == nil || !a.fitsHere() {
			return part{}, false
		}
		return part{ID: id, Variant: variant, Art: a}, true
	}
	names := make([]string, 0, len(vs))
	for name := range vs {
		names = append(names, name)
	}
	sort.Strings(names)
	var plain *part
	for _, name := range names {
		a := vs[name]
		if !a.fitsHere() {
			continue
		}
		if a.Needs == "" {
			if plain == nil {
				plain = &part{ID: id, Variant: name, Art: a}
			}
			continue
		}
		if p.meets(a.Needs) {
			return part{ID: id, Variant: name, Art: a}, true
		}
	}
	if plain != nil {
		return *plain, true
	}
	return part{}, false
}

// variantsHere returns the feature's variants for this OS, best first.
func (f *feature) variantsHere() []featureVariant {
	var out []featureVariant
	for _, v := range f.Variants {
		if v.OS == "" || v.OS == runtime.GOOS {
			out = append(out, v)
		}
	}
	return out
}

// pick chooses the feature's variant: the one named, or for "auto" or "" the first this machine meets the needs of. Output: the variant and its parts, or false when the name is unknown or nothing fits this platform.
func (m *manifest) pick(f *feature, name string, p Platform) (featureVariant, []part, bool) {
	for _, v := range f.variantsHere() {
		if name != "" && name != "auto" && v.Name != name {
			continue
		}
		if (name == "" || name == "auto") && v.Needs != "" && !p.meets(v.Needs) {
			continue
		}
		parts, ok := m.parts(v, p)
		if !ok {
			continue
		}
		return v, parts, true
	}
	return featureVariant{}, nil, false
}

// parts resolves every component a variant names.
func (m *manifest) parts(v featureVariant, p Platform) ([]part, bool) {
	var out []part
	for _, ref := range v.Components {
		pt, ok := m.resolve(ref, p)
		if !ok {
			return nil, false
		}
		out = append(out, pt)
	}
	return out, true
}

// mainPath is where a part's Main file lands, relative to the data directory with forward slashes.
func (pt part) mainPath() string {
	return path.Join(pt.Art.Dest, pt.Art.Main)
}

// componentIDs lists every component any variant of a feature names, once each.
func (m *manifest) componentIDs(f *feature) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range f.Variants {
		for _, ref := range v.Components {
			id, _, _ := strings.Cut(ref, "@")
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

// shared reports whether a feature other than f also uses the component.
func (m *manifest) shared(id string, f *feature) bool {
	for _, g := range m.Features {
		if g == f {
			continue
		}
		for _, other := range m.componentIDs(g) {
			if other == id {
				return true
			}
		}
	}
	return false
}
