package components

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A symbolic link whose target walks through another of the archive's links is refused. "sub" -> "." reads as inside, and "lnk" -> "sub/../escaped" cleans to "escaped", also inside, but the kernel follows "sub" to the staging directory itself and ".." then leaves it.
func TestExtract_RefusesALinkThatClimbsThroughAnotherLink(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, h := range []*tar.Header{
		{Name: "sub", Typeflag: tar.TypeSymlink, Linkname: "."},
		{Name: "lnk", Typeflag: tar.TypeSymlink, Linkname: "sub/../escaped"},
	} {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	archive := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := os.WriteFile(archive, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(t.TempDir(), "stage")
	if err := os.Mkdir(stage, 0o755); err != nil {
		t.Fatal(err)
	}

	err := extract(context.Background(), archive, stage, &artifact{Format: "tar.gz", Size: 1 << 20})

	if err == nil {
		t.Fatal("the archive was accepted, and lnk resolves to the folder above the staging directory")
	}
}
