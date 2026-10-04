package components

import (
	"archive/tar"
	"archive/zip"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// extractor unpacks one archive into a staging directory, keeping only what the artifact selects. Every entry's name is checked before anything is written, whether it is kept or not: the archive's hash was already checked against the pin, so an entry that would land outside the staging directory means the pin itself is wrong, and the whole archive is refused rather than the one entry.
type extractor struct {
	art     *artifact
	root    string
	limit   int64
	written int64
	files   map[string]bool
	links   []pendingLink
}

// pendingLink is a symbolic or hard link, made once every regular file is out, since an archive may list a link before the file it points at.
type pendingLink struct {
	name, target string
	hard         bool
}

// extract unpacks archivePath into dir. Input: the job's context, the verified archive, the empty staging directory, and the artifact saying how to read it. Output: the error that refused the archive or stopped the copy.
func extract(ctx context.Context, archivePath, dir string, a *artifact) error {
	x := &extractor{art: a, root: dir, files: map[string]bool{}}
	// A bound on what one archive may write, so a pin pointing at the wrong file cannot fill the disk: twice the expected size, with room for a small archive the manifest gives no size for.
	x.limit = 2*a.diskBytes() + 64<<20
	var err error
	switch a.Format {
	case "zip":
		err = x.zip(ctx, archivePath)
	case "tar.gz", "tar.bz2":
		err = x.tar(ctx, archivePath)
	default:
		err = fmt.Errorf("unknown archive format %q", a.Format)
	}
	if err != nil {
		return err
	}
	if err := x.checkThroughLinks(); err != nil {
		return err
	}
	return x.makeLinks()
}

// checkThroughLinks refuses an archive in which a path runs through one of its own symbolic links: a kept file or link whose directory is a link, or a link whose target's directory is one. link checks a target by joining it to the link's directory as written, which is where it really points only when no directory on the way is itself a link: "a" -> "." and then "a/x" -> "../evil" reads as staying inside, and once "a" is made it points one level above staging. The files are already out by now, but nothing has followed a link to write them, since links are only made after this.
func (x *extractor) checkThroughLinks() error {
	isLink := map[string]bool{}
	for _, l := range x.links {
		if !l.hard {
			isLink[l.name] = true
		}
	}
	if len(isLink) == 0 {
		return nil
	}
	through := func(p string) string {
		for d := path.Dir(p); d != "." && d != "/"; d = path.Dir(d) {
			if isLink[d] {
				return d
			}
		}
		return ""
	}
	for f := range x.files {
		if d := through(f); d != "" {
			return fmt.Errorf("archive entry %q is inside %q, which is a symbolic link", f, d)
		}
	}
	for _, l := range x.links {
		if d := through(l.name); d != "" {
			return fmt.Errorf("archive entry %q is inside %q, which is a symbolic link", l.name, d)
		}
		to := l.target
		if !l.hard {
			to = path.Join(path.Dir(l.name), l.target)
		}
		if d := through(to); d != "" {
			return fmt.Errorf("archive entry %q links through %q, which is a symbolic link", l.name, d)
		}
	}
	return nil
}

func (x *extractor) zip(ctx context.Context, archivePath string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		mode := f.Mode()
		if mode&fs.ModeSymlink != 0 {
			rc, err := f.Open()
			if err != nil {
				return err
			}
			target, err := io.ReadAll(io.LimitReader(rc, 4096))
			rc.Close()
			if err != nil {
				return err
			}
			if err := x.link(f.Name, string(target), false); err != nil {
				return err
			}
			continue
		}
		if mode.IsDir() || strings.HasSuffix(f.Name, "/") {
			if _, err := cleanName(f.Name); err != nil {
				return err
			}
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = x.file(f.Name, mode, rc)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func (x *extractor) tar(ctx context.Context, archivePath string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	var r io.Reader
	if x.art.Format == "tar.gz" {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gz.Close()
		r = gz
	} else {
		r = bzip2.NewReader(f)
	}
	tr := tar.NewReader(r)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeReg:
			if err := x.file(hdr.Name, hdr.FileInfo().Mode(), tr); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := x.link(hdr.Name, hdr.Linkname, false); err != nil {
				return err
			}
		case tar.TypeLink:
			if err := x.link(hdr.Name, hdr.Linkname, true); err != nil {
				return err
			}
		default:
			// Directories are made as files need them, and the pax and GNU metadata entries archive/tar hands back carry nothing to write; the name is still checked.
			if _, err := cleanName(hdr.Name); err != nil {
				return err
			}
		}
	}
}

// cleanName checks one entry's name and returns it with forward slashes. Output: an error for an absolute name, a drive-qualified one, or one with a ".." element.
func cleanName(name string) (string, error) {
	n := strings.ReplaceAll(name, `\`, "/")
	if strings.HasPrefix(n, "/") || (len(n) >= 2 && n[1] == ':') {
		return "", fmt.Errorf("archive entry %q has an absolute path", name)
	}
	for _, el := range strings.Split(n, "/") {
		if el == ".." {
			return "", fmt.Errorf("archive entry %q climbs out of its directory", name)
		}
	}
	return strings.TrimPrefix(path.Clean(n), "./"), nil
}

// target maps an archive entry to where it goes under the component's directory: Strip cut off, filtered by Include, renamed. Output: the relative path, or "" for an entry that is not kept.
func (x *extractor) target(name string) (string, error) {
	n, err := cleanName(name)
	if err != nil {
		return "", err
	}
	rel, ok := strings.CutPrefix(n, strings.TrimSuffix(x.art.Strip, "/"))
	if x.art.Strip != "" {
		if !ok || !strings.HasPrefix(rel, "/") {
			return "", nil
		}
		rel = rel[1:]
	}
	if rel == "" || rel == "." || !x.included(rel) {
		return "", nil
	}
	return x.renamed(rel), nil
}

func (x *extractor) included(rel string) bool {
	for _, pat := range x.art.Include {
		if ok, _ := path.Match(pat, rel); ok {
			return true
		}
	}
	return len(x.art.Include) == 0
}

// renamed applies the artifact's Rename: an exact name first, then the longest directory prefix.
func (x *extractor) renamed(rel string) string {
	if to, ok := x.art.Rename[rel]; ok {
		return to
	}
	best := ""
	for from := range x.art.Rename {
		if strings.HasSuffix(from, "/") && strings.HasPrefix(rel, from) && len(from) > len(best) {
			best = from
		}
	}
	if best == "" {
		return rel
	}
	return x.art.Rename[best] + strings.TrimPrefix(rel, best)
}

// file writes one kept regular file. Execute permission survives, which is what the Linux builds need; everything else is the owner's to read and write.
func (x *extractor) file(name string, mode fs.FileMode, r io.Reader) error {
	rel, err := x.target(name)
	if err != nil || rel == "" {
		return err
	}
	dst := filepath.Join(x.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	perm := fs.FileMode(0o644)
	if mode&0o111 != 0 {
		perm = 0o755
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(r, x.limit-x.written+1))
	x.written += n
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if x.written > x.limit {
		return fmt.Errorf("the archive unpacks to more than the %d bytes expected of it", x.limit)
	}
	x.files[rel] = true
	return nil
}

// link records a link to make once the files are out. A symbolic link may only point at another path inside the staging directory, and a hard link only at another entry of the same archive.
func (x *extractor) link(name, target string, hard bool) error {
	rel, err := x.target(name)
	if err != nil || rel == "" {
		return err
	}
	if hard {
		to, err := x.target(target)
		if err != nil {
			return err
		}
		if to == "" {
			return fmt.Errorf("archive entry %q links to %q, which is not kept", name, target)
		}
		x.links = append(x.links, pendingLink{name: rel, target: to, hard: true})
		return nil
	}
	t := strings.ReplaceAll(target, `\`, "/")
	if strings.HasPrefix(t, "/") || (len(t) >= 2 && t[1] == ':') {
		return fmt.Errorf("archive entry %q links to the absolute path %q", name, target)
	}
	resolved := path.Join(path.Dir(rel), t)
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return fmt.Errorf("archive entry %q links to %q, outside its directory", name, target)
	}
	x.links = append(x.links, pendingLink{name: rel, target: t})
	return nil
}

// makeLinks makes the links recorded during the copy. A symbolic link that cannot be made — Windows only lets an elevated or developer-mode user make one — becomes a copy of what it points at, which is all a link between two libraries in the same directory is for. Copies are retried until no more resolve, since one link may point at another.
func (x *extractor) makeLinks() error {
	pending := x.links
	for len(pending) > 0 {
		var later []pendingLink
		for _, l := range pending {
			dst := filepath.Join(x.root, filepath.FromSlash(l.name))
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			to := l.target
			if !l.hard {
				if err := os.Symlink(filepath.FromSlash(l.target), dst); err == nil {
					x.files[l.name] = true
					continue
				}
				to = path.Join(path.Dir(l.name), l.target)
			}
			if !x.files[to] {
				later = append(later, l)
				continue
			}
			if err := copyFile(filepath.Join(x.root, filepath.FromSlash(to)), dst); err != nil {
				return err
			}
			x.files[l.name] = true
		}
		if len(later) == len(pending) {
			// Links to links the archive never delivered: they point at nothing, so they are left out rather than made dangling.
			return nil
		}
		pending = later
	}
	return nil
}

// copyFile copies src to dst with src's permissions.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
