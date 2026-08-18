package tracker

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func TestEncodeJPEG_DownsamplesWidePNG(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2000, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 2000; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: 10, B: 10, A: 255})
		}
	}
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		t.Fatal(err)
	}
	out := encodeJPEG(pngBuf.Bytes())
	if len(out) == 0 {
		t.Fatal("expected jpeg bytes")
	}
	decoded, _, err := image.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Bounds().Dx() != jpegMaxWidth {
		t.Fatalf("width=%d want %d", decoded.Bounds().Dx(), jpegMaxWidth)
	}
	// 2000x100 noisy strip at the memory-thumbnail settings should stay well under 80KB.
	if len(out) > 80*1024 {
		t.Fatalf("jpeg too large for a thumbnail: %d bytes", len(out))
	}
}

func TestEncodeJPEG_Empty(t *testing.T) {
	if encodeJPEG(nil) != nil {
		t.Fatal("expected nil")
	}
}
