package tracker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	shotUser32                 = windows.NewLazySystemDLL("user32.dll")
	shotGdi32                  = windows.NewLazySystemDLL("gdi32.dll")
	procGetSystemMetrics       = shotUser32.NewProc("GetSystemMetrics")
	procGetDC                  = shotUser32.NewProc("GetDC")
	procReleaseDC              = shotUser32.NewProc("ReleaseDC")
	procGetCursorPos           = shotUser32.NewProc("GetCursorPos")
	procEnumDisplayMonitors    = shotUser32.NewProc("EnumDisplayMonitors")
	procCreateCompatibleDC     = shotGdi32.NewProc("CreateCompatibleDC")
	procCreateCompatibleBitmap = shotGdi32.NewProc("CreateCompatibleBitmap")
	procSelectObject           = shotGdi32.NewProc("SelectObject")
	procBitBlt                 = shotGdi32.NewProc("BitBlt")
	procGetDIBits              = shotGdi32.NewProc("GetDIBits")
	procDeleteObject           = shotGdi32.NewProc("DeleteObject")
	procDeleteDC               = shotGdi32.NewProc("DeleteDC")
)

// GetSystemMetrics indexes for the virtual desktop: the rectangle spanning every monitor, whose top-left corner is negative when a monitor sits left of or above the primary one.
const (
	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCXVirtualScreen = 78
	smCYVirtualScreen = 79
)

// srcCopy is BitBlt's plain copy raster operation. CAPTUREBLT is left off because it makes the cursor flicker on every grab, and under DWM on Windows 8 and later a plain copy already includes layered windows.
const srcCopy = 0x00CC0020

// bitmapInfo is Win32's BITMAPINFO for a 32-bit uncompressed bitmap: the BITMAPINFOHEADER plus the one RGBQUAD the struct declares, unused at this depth.
type bitmapInfo struct {
	biSize          uint32
	biWidth         int32
	biHeight        int32
	biPlanes        uint16
	biBitCount      uint16
	biCompression   uint32
	biSizeImage     uint32
	biXPelsPerMeter int32
	biYPelsPerMeter int32
	biClrUsed       uint32
	biClrImportant  uint32
	bmiColors       [4]byte
}

// WarmUpScreenshotPermission is a no-op on Windows: GDI capture asks nobody for consent.
func WarmUpScreenshotPermission(ctx context.Context) {}

// virtualDesktop reads the rectangle spanning every monitor, in physical pixels (the process is per-monitor DPI aware, see dpi_windows.go). Output: the rectangle, whose Min is where a screenshot's pixel 0,0 sits on the desktop.
func virtualDesktop() image.Rectangle {
	metric := func(i uintptr) int {
		v, _, _ := procGetSystemMetrics.Call(i)
		return int(int32(v))
	}
	x, y := metric(smXVirtualScreen), metric(smYVirtualScreen)
	return image.Rect(x, y, x+metric(smCXVirtualScreen), y+metric(smCYVirtualScreen))
}

// grabVirtual captures every monitor at once with GDI. Output: the picture with its own 0,0 at the top-left, and the desktop point that pixel 0,0 is, which callers add back to turn a picture point into a screen point; or an error when any GDI call fails, which is what happens on the lock screen.
func grabVirtual() (*image.RGBA, image.Point, error) {
	desk := virtualDesktop()
	w, h := desk.Dx(), desk.Dy()
	if w <= 0 || h <= 0 {
		return nil, image.Point{}, errors.New("no virtual desktop to capture")
	}

	screen, _, _ := procGetDC.Call(0)
	if screen == 0 {
		return nil, image.Point{}, errors.New("GetDC failed")
	}
	defer procReleaseDC.Call(0, screen) //nolint:errcheck

	mem, _, _ := procCreateCompatibleDC.Call(screen)
	if mem == 0 {
		return nil, image.Point{}, errors.New("CreateCompatibleDC failed")
	}
	defer procDeleteDC.Call(mem) //nolint:errcheck

	bmp, _, _ := procCreateCompatibleBitmap.Call(screen, uintptr(w), uintptr(h))
	if bmp == 0 {
		return nil, image.Point{}, errors.New("CreateCompatibleBitmap failed")
	}
	defer procDeleteObject.Call(bmp) //nolint:errcheck

	old, _, _ := procSelectObject.Call(mem, bmp)
	ok, _, _ := procBitBlt.Call(mem, 0, 0, uintptr(w), uintptr(h), screen, uintptr(desk.Min.X), uintptr(desk.Min.Y), srcCopy)
	// GetDIBits must not be called on a bitmap that is still selected into a DC, so it is put back before the read whether or not the copy worked.
	procSelectObject.Call(mem, old) //nolint:errcheck
	if ok == 0 {
		return nil, image.Point{}, errors.New("BitBlt failed")
	}

	// A negative height asks for the rows top-down, the order image.RGBA keeps them in.
	info := bitmapInfo{biWidth: int32(w), biHeight: -int32(h), biPlanes: 1, biBitCount: 32}
	info.biSize = uint32(unsafe.Offsetof(info.bmiColors))
	pix := make([]byte, 4*w*h)
	if lines, _, _ := procGetDIBits.Call(screen, bmp, 0, uintptr(h), uintptr(unsafe.Pointer(&pix[0])), uintptr(unsafe.Pointer(&info)), 0); int(lines) != h {
		return nil, image.Point{}, fmt.Errorf("GetDIBits copied %d of %d rows", lines, h)
	}
	return bgraToRGBA(pix, w, h), desk.Min, nil
}

// grabScreen returns a PNG of every monitor for the vision tier, the same contract as on Linux. Pixel 0,0 is the virtual desktop's top-left corner, which screenLayout accounts for.
func grabScreen(_ context.Context) ([]byte, error) {
	defer standAside()()
	img, _, err := grabVirtual()
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	// BestSpeed: this PNG is decoded again in the same process moments later, so smaller bytes buy nothing and the default level costs several times the time on a large desktop.
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("encode screenshot: %w", err)
	}
	return buf.Bytes(), nil
}

// monitorRects collects the rectangles EnumDisplayMonitors reports. The callback is made once because syscall.NewCallback slots are never freed and the process can hold only about two thousand; the mutex keeps two screenLayout calls from writing into one list.
var (
	monitorMu    sync.Mutex
	monitorRects []image.Rectangle
	monitorEnum  = syscall.NewCallback(func(_, _ uintptr, rect *windows.Rect, _ uintptr) uintptr {
		monitorRects = append(monitorRects, image.Rect(int(rect.Left), int(rect.Top), int(rect.Right), int(rect.Bottom)))
		return 1
	})
)

// screenLayout reports the monitor rectangles and a point on the monitor the user is working on, both in the screenshot's own coordinates: the virtual-desktop position minus the desktop's top-left corner, so a monitor left of the primary does not come out negative.
// The point is the centre of the window in front, which says which monitor the user is on better than the pointer does; the pointer stands in when no window has focus. Output: (nil, (-1,-1)) when no monitor is reported.
func screenLayout() ([]image.Rectangle, image.Point) {
	unknown := image.Pt(-1, -1)
	origin := virtualDesktop().Min

	monitorMu.Lock()
	monitorRects = nil
	ok, _, _ := procEnumDisplayMonitors.Call(0, 0, monitorEnum, 0)
	mons := monitorRects
	monitorRects = nil
	monitorMu.Unlock()
	if ok == 0 || len(mons) == 0 {
		return nil, unknown
	}
	for i := range mons {
		mons[i] = mons[i].Sub(origin)
	}

	if frame, ok := windowFrame(windows.GetForegroundWindow()); ok {
		c := frame.Sub(origin)
		return mons, image.Pt((c.Min.X+c.Max.X)/2, (c.Min.Y+c.Max.Y)/2)
	}
	var p struct{ X, Y int32 }
	if r, _, _ := procGetCursorPos.Call(uintptr(unsafe.Pointer(&p))); r != 0 {
		return mons, image.Pt(int(p.X), int(p.Y)).Sub(origin)
	}
	return mons, unknown
}
