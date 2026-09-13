#!/usr/bin/env bash
# Installs a staged Ora build (the directory this script lives in) into the current user's XDG directories and starts it as a systemd user service. Never touches anything outside $HOME.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

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

mkdir -p "$bin" "$apps" "$units" "$extdir"
install -m 0755 "$here/ora" "$bin/ora"
install -m 0755 "$here/ora-window" "$bin/ora-window"
# Exec= is rewritten to the absolute path of the installed binary. The entry ships saying `Exec=ora`, and a desktop launching it that way finds nothing: ~/.local/bin only joins PATH when ~/.profile runs at login and the directory already exists, which on a machine installing Ora for the first time it did not.
sed "s|^Exec=ora$|Exec=$bin/ora|" "$here/ora.desktop" > "$apps/ora.desktop"
chmod 0644 "$apps/ora.desktop"
install -m 0644 "$here/ora.service" "$units/ora.service"
install -m 0644 "$here/gnome-extension/ora@ora.local/metadata.json" "$extdir/metadata.json"
install -m 0644 "$here/gnome-extension/ora@ora.local/extension.js" "$extdir/extension.js"

for size_dir in "$here"/icons/hicolor/*/apps; do
	size="$(basename "$(dirname "$size_dir")")"
	dest="$icons/$size/apps"
	mkdir -p "$dest"
	install -m 0644 "$size_dir/ora.png" "$dest/ora.png"
done

command -v update-desktop-database >/dev/null 2>&1 && update-desktop-database "$apps" || true
command -v gtk-update-icon-cache >/dev/null 2>&1 && gtk-update-icon-cache -f -t "$icons" || true

if command -v gnome-extensions >/dev/null 2>&1; then
	gnome-extensions enable ora@ora.local >/dev/null 2>&1 || true
	echo "Window-raising extension installed: on Wayland, log out and back in once to finish enabling it."
fi

systemctl --user daemon-reload
systemctl --user enable --now ora.service

echo "Ora installed. Check it with: systemctl --user status ora.service"
