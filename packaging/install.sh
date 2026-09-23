#!/usr/bin/env bash
# Installs a staged Ora build (the directory this script lives in) into the current user's XDG directories and starts it as a systemd user service. Never touches anything outside $HOME.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "Computer use needs one logout and login after this install, so GNOME loads Ora's extension; after that, run: ora doctor"

bin="$HOME/.local/bin"
apps="$HOME/.local/share/applications"
icons="$HOME/.local/share/icons/hicolor"
units="$HOME/.config/systemd/user"
extdir="$HOME/.local/share/gnome-shell/extensions/ora@ora.local"

# The window is a Tauri binary linked against the system WebKitGTK stack, which a fresh desktop often does not have. Checked before anything is installed, so a missing library reads as a message here instead of as a window that never opens.
missing="$(ldd "$here/ora-window" 2>/dev/null | awk '/not found/ {print $1}' || true)"
if [ -n "$missing" ]; then
	echo "Ora's window needs shared libraries this machine does not have:" >&2
	echo "$missing" | sed 's/^/  /' >&2
	echo "On Debian or Ubuntu: sudo apt install libwebkit2gtk-4.1-0 libgtk-3-0 libsoup-3.0-0" >&2
	exit 1
fi

# Computer use (seeing and clicking the screen) is GNOME-only: the window frames it needs on Wayland come from the bundled GNOME Shell extension, and no other desktop loads a GNOME Shell extension at all. This is a degraded install, not a failed one — Ora still runs and can chat on any desktop — so it is reported rather than aborted, and the extension is not installed into a directory nothing on this desktop reads.
desktop="${XDG_CURRENT_DESKTOP:-unknown}"
case "$desktop" in
*GNOME*) is_gnome=true ;;
*) is_gnome=false ;;
esac
if [ "$is_gnome" = false ]; then
	echo "Desktop environment reports '$desktop', not GNOME: computer use needs GNOME Shell to load the window-raising extension, so Ora will not be able to see window positions or place clicks here. Chat and voice still work." >&2
fi

mkdir -p "$bin" "$apps" "$units"
install -m 0755 "$here/ora" "$bin/ora"
install -m 0755 "$here/ora-window" "$bin/ora-window"
# Exec= is rewritten to the absolute path of the installed binary. The entry ships saying `Exec=ora`, and a desktop launching it that way finds nothing: ~/.local/bin only joins PATH when ~/.profile runs at login and the directory already exists, which on a machine installing Ora for the first time it did not.
sed "s|^Exec=ora$|Exec=$bin/ora|" "$here/ora.desktop" > "$apps/ora.desktop"
chmod 0644 "$apps/ora.desktop"
install -m 0644 "$here/ora.service" "$units/ora.service"

if [ "$is_gnome" = true ]; then
	mkdir -p "$extdir"
	install -m 0644 "$here/gnome-extension/ora@ora.local/metadata.json" "$extdir/metadata.json"
	install -m 0644 "$here/gnome-extension/ora@ora.local/extension.js" "$extdir/extension.js"
fi

for size_dir in "$here"/icons/hicolor/*/apps; do
	size="$(basename "$(dirname "$size_dir")")"
	dest="$icons/$size/apps"
	mkdir -p "$dest"
	install -m 0644 "$size_dir/ora.png" "$dest/ora.png"
done

command -v update-desktop-database >/dev/null 2>&1 && update-desktop-database "$apps" || true
command -v gtk-update-icon-cache >/dev/null 2>&1 && gtk-update-icon-cache -f -t "$icons" || true

if [ "$is_gnome" = true ] && command -v gnome-extensions >/dev/null 2>&1; then
	# check-shell-version.sh reads gnome-shell's own version against metadata.json's shell-version list: outside that list gnome-shell silently refuses to load the extension, with no error dialog, so the version and the consequence are stated here rather than left to be discovered as window frames that never appear.
	if [ -x "$here/gnome-extension/check-shell-version.sh" ]; then
		"$here/gnome-extension/check-shell-version.sh" || true
	fi
	gnome-extensions enable ora@ora.local >/dev/null 2>&1 || true
	echo "Window-raising extension installed: on Wayland, log out and back in once to finish enabling it."
fi

systemctl --user daemon-reload
systemctl --user enable --now ora.service

echo "Ora installed."
# ora doctor is the one place every prerequisite computer use needs is checked together; running it here means a machine that cannot actually see or drive the screen finds that out now, not the first time it tries to click something. The daemon was just started, so give it a moment to come up before asking it to report on itself.
sleep 1
echo
"$bin/ora" doctor
