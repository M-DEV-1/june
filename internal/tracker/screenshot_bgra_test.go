package tracker

import (
	"image"
	"image/color"
	"testing"
)

// A Windows screen grab arrives as blue, green, red, unused bytes with the unused byte at 0; read as RGBA unchanged it would come out blue-tinted and fully transparent, and the PNG the vision tier gets would be blank.
func TestBGRAToRGBA(t *testing.T) {
	pix := []byte{
		10, 20, 30, 0, 40, 50, 60, 0,
		70, 80, 90, 0, 1, 2, 3, 0,
	}
	img := bgraToRGBA(pix, 2, 2)
	if img.Bounds() != image.Rect(0, 0, 2, 2) {
		t.Fatalf("bounds %v, want 2x2", img.Bounds())
	}
	if got, want := img.RGBAAt(1, 0), (color.RGBA{R: 60, G: 50, B: 40, A: 255}); got != want {
		t.Errorf("pixel (1,0) = %v, want %v", got, want)
	}
	if got, want := img.RGBAAt(0, 1), (color.RGBA{R: 90, G: 80, B: 70, A: 255}); got != want {
		t.Errorf("pixel (0,1) = %v, want %v", got, want)
	}
}
