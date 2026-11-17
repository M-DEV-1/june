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

# The shortcut install.sh registered with GNOME, found by the command it runs, so any other custom keybinding stays in the list as it was.
if command -v gsettings >/dev/null 2>&1; then
	media_keys=org.gnome.settings-daemon.plugins.media-keys
	list="$(gsettings get "$media_keys" custom-keybindings 2>/dev/null || true)"
	kept=""
	removed=false
	for path in $(grep -o "'[^']*'" <<<"$list" | tr -d "'"); do
		case "$(gsettings get "$media_keys.custom-keybinding:$path" command 2>/dev/null || true)" in
		*june-window-toggle*)
			gsettings reset-recursively "$media_keys.custom-keybinding:$path" || true
			removed=true
			;;
		*) kept="${kept:+$kept, }'$path'" ;;
		esac
	done
	if [ "$removed" = true ]; then
		remaining="@as []"
		if [ -n "$kept" ]; then
			remaining="[$kept]"
		fi
		gsettings set "$media_keys" custom-keybindings "$remaining" || true
	fi
fi

rm -f "$HOME/.local/bin/june" "$HOME/.local/bin/june-window" "$HOME/.local/bin/june-window-toggle"
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
