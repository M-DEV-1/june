//go:build windows

package tracker

import "image"

// bgraToRGBA turns the pixels a Windows GDI screen grab returns into an image, reusing the same bytes. Input: the bytes GetDIBits wrote for a top-down 32-bit bitmap, four per pixel in blue, green, red, unused order, and the bitmap's width and height. Output: an RGBA image over those bytes with red and blue swapped and alpha set to 255, because GDI leaves the fourth byte at 0.
// It lives in a file every platform builds so its test runs on Linux, where the rest of the Windows capture cannot.
func bgraToRGBA(pix []byte, w, h int) *image.RGBA {
	for i := 0; i+3 < len(pix); i += 4 {
		pix[i], pix[i+2], pix[i+3] = pix[i+2], pix[i], 255
	}
	return &image.RGBA{Pix: pix, Stride: 4 * w, Rect: image.Rect(0, 0, w, h)}
}
