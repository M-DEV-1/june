#!/usr/bin/env bash
# Reverses install.sh: stops the service and removes every file it put under $HOME. Never touches anything outside $HOME.
set -euo pipefail

# Every path below is under $HOME, so an empty or unset $HOME would turn each of them into a root-relative path. Refuse before anything is removed. set -u catches the unset case; this catches the empty one too, which -u lets through.
if [ -z "${HOME:-}" ]; then
	echo "HOME is not set, so there is no Ora installation to remove. Nothing was deleted." >&2
	exit 1
fi

systemctl --user disable --now ora.service >/dev/null 2>&1 || true
systemctl --user daemon-reload

command -v gnome-extensions >/dev/null 2>&1 && gnome-extensions disable ora@ora.local >/dev/null 2>&1 || true
rm -rf "$HOME/.local/share/gnome-shell/extensions/ora@ora.local"

rm -f "$HOME/.local/bin/ora" "$HOME/.local/bin/ora-window"
# Both entries: the packaged one, and the hidden ora-overlay.desktop the daemon writes itself so GNOME keeps the always-mapped overlay window out of Ora's dock entry. Leaving the second one behind left a stale entry pointing at a binary that is gone.
rm -f "$HOME/.local/share/applications/ora.desktop" "$HOME/.local/share/applications/ora-overlay.desktop"
rm -f "$HOME/.config/systemd/user/ora.service"
for size_dir in "$HOME"/.local/share/icons/hicolor/*/apps; do
	rm -f "$size_dir/ora.png"
done

command -v update-desktop-database >/dev/null 2>&1 && update-desktop-database "$HOME/.local/share/applications" || true
command -v gtk-update-icon-cache >/dev/null 2>&1 && gtk-update-icon-cache -f -t "$HOME/.local/share/icons/hicolor" || true

echo "Ora uninstalled."
