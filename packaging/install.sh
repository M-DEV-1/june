#!/usr/bin/env bash
# Installs a staged Ora build (the directory this script lives in) into the current user's XDG directories and starts it as a systemd user service. Never touches anything outside $HOME.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

bin="$HOME/.local/bin"
apps="$HOME/.local/share/applications"
icons="$HOME/.local/share/icons/hicolor"
units="$HOME/.config/systemd/user"
extdir="$HOME/.local/share/gnome-shell/extensions/ora@ora.local"

mkdir -p "$bin" "$apps" "$units" "$extdir"
install -m 0755 "$here/ora" "$bin/ora"
install -m 0755 "$here/ora-window" "$bin/ora-window"
install -m 0644 "$here/ora.desktop" "$apps/ora.desktop"
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
