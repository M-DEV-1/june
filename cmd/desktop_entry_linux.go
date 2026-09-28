//go:build linux

package cmd

import (
	"bytes"
	_ "embed"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"

	"june/internal/util"
)

// appIconPNG is June's logo at 512x512, the largest raster the repo has (app/src-tauri/icons/icon.png, the same art the Tauri bundle ships). It is the source every dock icon size is scaled down from; there is no SVG of the logo anywhere in the repo, so no scalable icon is installed.
//
//go:embed app_icon_linux.png
var appIconPNG []byte

// dockIconSizes are the square pixel sizes June's icon is written at, one per standard hicolor directory a desktop looks in. A theme lookup picks the nearest size to what it is drawing, so a dock asking for 22 or 96 pixels now scales a nearby real file instead of the 128 pixel one that used to be the only file on disk.
var dockIconSizes = []int{16, 32, 48, 64, 128, 256}

// iconCacheRefresh runs gtk-update-icon-cache over an icon theme directory. It is a variable so the tests can record the call instead of shelling out to the real program.
var iconCacheRefresh = runGTKUpdateIconCache

// desktopEntryMarker is a comment line this daemon writes into every june.desktop it owns, so a later run can tell its own dev-time entry apart from a real one a package's install.sh put there instead. A comment is invisible to every Desktop Entry parser, so its presence changes nothing about how the entry behaves.
const desktopEntryMarker = "# Written by the june daemon itself; delete this file to use an installed package's entry instead."

// installDesktopEntry writes June's dock icon at every size a desktop asks for and the .desktop entries naming it, so GNOME shows the June logo instead of the theme's missing-image mark for the Tauri window (WM_CLASS "june"/"June").
// It writes the logo to $XDG_DATA_HOME/icons/hicolor/<size>/apps/june.png for each size in dockIconSizes, an entry naming Icon=june and StartupWMClass=june to $XDG_DATA_HOME/applications/june.desktop, and a hidden entry claiming StartupWMClass=june-overlay to june-overlay.desktop; StartupWMClass is what lets GNOME match a running window to an entry, and the second entry is what keeps the always-mapped overlay window out of June's own dock entry (see overlayDesktopEntry).
// The application entry is skipped entirely when one already exists there without desktopEntryMarker in it: that means a real package (see packaging/june.desktop, installed by install.sh) put its own entry down, and a dev build must never overwrite it.
// Each file that is written is written only when its content differs from what is already on disk, so a daemon restart does not rewrite any of them. When an icon file did change, or when the theme has no icon-theme.cache at all, gtk-update-icon-cache is run over the hicolor directory, because a desktop that reads that cache never sees a file the cache does not list.
// Output: an error from a directory create, a stat, a decode, os.Executable, a write, or the cache refresh; the caller logs it and continues, since a missing dock icon should never stop the daemon.
func installDesktopEntry() error {
	dataHome := util.DataHome()
	if dataHome == "" {
		return fmt.Errorf("cannot determine XDG data directory")
	}

	themeDir := filepath.Join(dataHome, "icons", "hicolor")
	iconsChanged, err := installDockIcons(themeDir)
	if err != nil {
		return err
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	dir := filepath.Dir(exe)
	appsDir := filepath.Join(dataHome, "applications")
	appPath := filepath.Join(appsDir, "june.desktop")
	if desktopEntryIsOurs(appPath) {
		if _, err := writeFileIfChanged(appPath, []byte(applicationDesktopEntry(exe, dir))); err != nil {
			return err
		}
	}
	if _, err := writeFileIfChanged(filepath.Join(appsDir, "june-overlay.desktop"), []byte(overlayDesktopEntry(exe, dir))); err != nil {
		return err
	}

	if !iconsChanged && iconCacheExists(themeDir) {
		return nil
	}
	return iconCacheRefresh(themeDir)
}

// installDockIcons writes June's logo into themeDir at every size in dockIconSizes, as themeDir/<size>x<size>/apps/june.png, scaling the embedded 512 pixel master down to each one.
// Input: the hicolor theme directory. Output: whether any file on disk changed, and an error from decoding the master, encoding a size, or writing a file.
func installDockIcons(themeDir string) (bool, error) {
	master, err := png.Decode(bytes.NewReader(appIconPNG))
	if err != nil {
		return false, fmt.Errorf("decode embedded app icon: %w", err)
	}

	changed := false
	for _, size := range dockIconSizes {
		var encoded bytes.Buffer
		if err := png.Encode(&encoded, scaleIcon(master, size)); err != nil {
			return changed, fmt.Errorf("encode %dx%d app icon: %w", size, size, err)
		}
		path := filepath.Join(themeDir, fmt.Sprintf("%dx%d", size, size), "apps", "june.png")
		wrote, err := writeFileIfChanged(path, encoded.Bytes())
		if err != nil {
			return changed, err
		}
		changed = changed || wrote
	}
	return changed, nil
}

// scaleIcon shrinks an image to size by size pixels by averaging each destination pixel over the block of source pixels it covers, which is the right filter for making a small icon out of a large one: dropping pixels instead would break up thin strokes.
// The average is weighted by alpha, so a pixel that is nearly transparent contributes almost none of its colour and a transparent edge does not bleed a dark halo into the icon.
// Input: the source image and the square size to write. Output: a new non-premultiplied RGBA image of that size.
func scaleIcon(src image.Image, size int) *image.NRGBA {
	bounds := src.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		y0 := bounds.Min.Y + y*bounds.Dy()/size
		y1 := bounds.Min.Y + (y+1)*bounds.Dy()/size
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < size; x++ {
			x0 := bounds.Min.X + x*bounds.Dx()/size
			x1 := bounds.Min.X + (x+1)*bounds.Dx()/size
			if x1 <= x0 {
				x1 = x0 + 1
			}

			// The colour sums are premultiplied by alpha (that is what image.Color.RGBA returns), so dividing them by the alpha sum at the end gives the alpha-weighted average colour back in non-premultiplied form.
			var sumR, sumG, sumB, sumA uint64
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					r, g, b, a := src.At(sx, sy).RGBA()
					sumR += uint64(r)
					sumG += uint64(g)
					sumB += uint64(b)
					sumA += uint64(a)
				}
			}

			count := uint64((y1 - y0) * (x1 - x0))
			i := dst.PixOffset(x, y)
			dst.Pix[i+3] = uint8(sumA / count >> 8)
			if sumA == 0 {
				continue
			}
			dst.Pix[i+0] = uint8(sumR * 0xff / sumA)
			dst.Pix[i+1] = uint8(sumG * 0xff / sumA)
			dst.Pix[i+2] = uint8(sumB * 0xff / sumA)
		}
	}
	return dst
}

// desktopEntryIsOurs reports whether the daemon may write its own june.desktop at path: true when nothing is there yet, or when what is there already carries desktopEntryMarker. False means something else — a package's own install — put a real entry there, which the daemon must leave alone.
func desktopEntryIsOurs(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return true
	}
	return bytes.Contains(data, []byte(desktopEntryMarker))
}

// iconCacheExists reports whether an icon theme directory already has a gtk-update-icon-cache file in it.
func iconCacheExists(themeDir string) bool {
	info, err := os.Stat(filepath.Join(themeDir, "icon-theme.cache"))
	return err == nil && info.Mode().IsRegular()
}

// runGTKUpdateIconCache rebuilds themeDir/icon-theme.cache so a desktop that reads the cache sees the icon files just written; GNOME Shell keeps whatever it looked up before, so an icon added under a cache it already read stays missing until the cache is written again.
// It passes --ignore-theme-index because a user's own hicolor directory carries no index.theme (the system copy at /usr/share/icons/hicolor supplies the directory list), and --force because the cache is being rewritten precisely when gtk-update-icon-cache would judge it already current.
// Input: the hicolor theme directory. Output: nil when the cache was written, and nil when gtk-update-icon-cache is not installed at all, since there is then no cache for anything to read; an error only when the program ran and failed.
func runGTKUpdateIconCache(themeDir string) error {
	path, err := exec.LookPath("gtk-update-icon-cache")
	if err != nil {
		return nil
	}
	out, err := exec.Command(path, "--force", "--ignore-theme-index", "--quiet", themeDir).CombinedOutput()
	if err != nil {
		return fmt.Errorf("gtk-update-icon-cache %s: %w: %s", themeDir, err, bytes.TrimSpace(out))
	}
	return nil
}

// writeFileIfChanged writes data to path, creating parent directories (0755) and the file (0644), but only when the file does not already exist with identical content — a fresh write would rewrite its own mtime, and this is what lets the caller run on every daemon start without touching disk each time.
// Output: whether the file was written, and an error from the directory create or the write.
func writeFileIfChanged(path string, data []byte) (bool, error) {
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, data) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return false, err
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return false, err
	}
	return true, nil
}

// applicationDesktopEntry renders the .desktop file body that names June's dock icon and lets GNOME match the running window to this entry via StartupWMClass, escaping exe and dir per the Desktop Entry Specification's quoting rules (see desktopEntryQuoteExec, desktopEntryEscapeString in autostart_linux.go).
func applicationDesktopEntry(exe, dir string) string {
	return fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=June
Comment=June
Exec=%s --daemon
Path=%s
Icon=june
Terminal=false
StartupWMClass=june
NoDisplay=false
Categories=Utility;
%s
`, desktopEntryQuoteExec(exe), desktopEntryEscapeString(dir), desktopEntryMarker)
}

// overlayDesktopEntry renders the hidden .desktop file that claims the overlay window's WM_CLASS instance name, "june-overlay" (set in app/src-tauri/src/lib.rs), so GNOME files that window under an application of its own rather than under June's.
// This is what keeps the dock's window count honest. GNOME Shell puts every window of an application in one list whether or not the window asks to skip the taskbar (shell-app.c only counts skip-taskbar windows out of the running/stopped decision, not out of the list), and a dock that draws one dot per window in that list therefore draws a dot for the full-screen drawing layer that is mapped the whole time June runs. Matched to this entry instead, the layer becomes an application whose only window skips the taskbar, which never reaches the running state and so never gets a dock entry of its own.
// NoDisplay keeps it out of the app grid and out of search. Without a file whose name matches the window's instance name, GNOME would fall back to matching the window by process id and file it under June again.
func overlayDesktopEntry(exe, dir string) string {
	return fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=June overlay
Comment=June's on-screen drawing layer
Exec=%s --daemon
Path=%s
Icon=june
Terminal=false
StartupWMClass=june-overlay
NoDisplay=true
Categories=Utility;
`, desktopEntryQuoteExec(exe), desktopEntryEscapeString(dir))
}
