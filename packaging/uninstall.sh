#!/usr/bin/env bash
# Reverses install.sh: stops June and removes every file it put under $HOME. Never touches anything outside $HOME.
set -euo pipefail

# Every path below is under $HOME, so an empty or unset $HOME would turn each of them into a root-relative path. Refuse before anything is removed. set -u catches the unset case; this catches the empty one too, which -u lets through.
if [ -z "${HOME:-}" ]; then
	echo "HOME is not set, so there is no June installation to remove. Nothing was deleted." >&2
	exit 1
fi

# The service is what install.sh up to 0.1.0 set up; a June started any other way is asked to quit, since one left running after its files are gone keeps recording with nothing on disk to stop or update it.
systemctl --user disable --now june.service >/dev/null 2>&1 || true
systemctl --user daemon-reload >/dev/null 2>&1 || true
if [ -x "$HOME/.local/bin/june" ]; then
	"$HOME/.local/bin/june" --quit || true
fi

command -v gnome-extensions >/dev/null 2>&1 && gnome-extensions disable june@june.local >/dev/null 2>&1 || true
rm -rf "$HOME/.local/share/gnome-shell/extensions/june@june.local"

rm -f "$HOME/.local/bin/june" "$HOME/.local/bin/june-window"
# Both entries: the packaged one, and the hidden june-overlay.desktop the daemon writes itself so GNOME keeps the always-mapped overlay window out of June's dock entry. Leaving the second one behind left a stale entry pointing at a binary that is gone.
rm -f "$HOME/.local/share/applications/june.desktop" "$HOME/.local/share/applications/june-overlay.desktop"
rm -f "$HOME/.config/systemd/user/june.service"
# install.sh's stop timeout for the unit systemd makes from the start-at-login entry.
rm -rf "${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/app-june@autostart.service.d"
# The start-at-login entry June's settings write (cmd/autostart_linux.go); left behind, every login would try to start a binary that is gone.
rm -f "${XDG_CONFIG_HOME:-$HOME/.config}/autostart/june.desktop"
for size_dir in "$HOME"/.local/share/icons/hicolor/*/apps; do
	rm -f "$size_dir/june.png"
done

command -v update-desktop-database >/dev/null 2>&1 && update-desktop-database "$HOME/.local/share/applications" || true
command -v gtk-update-icon-cache >/dev/null 2>&1 && gtk-update-icon-cache -f -t "$HOME/.local/share/icons/hicolor" || true

echo "June uninstalled."
