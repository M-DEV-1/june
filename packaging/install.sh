#!/usr/bin/env bash
# Installs a staged June build (the directory this script lives in) into the current user's XDG directories, has a first install start June at login, and opens June's window, where first-run setup takes over. Never touches anything outside $HOME.
# JUNE_NO_SERVICE=1 installs the files only: nothing is started or stopped and start at login is left alone, for CI and other machines with no graphical session, which then start `june --daemon` themselves.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "Computer use needs one logout and login after this install, so GNOME loads June's extension; after that, run: june doctor"

bin="$HOME/.local/bin"
apps="$HOME/.local/share/applications"
icons="$HOME/.local/share/icons/hicolor"
units="$HOME/.config/systemd/user"
extdir="$HOME/.local/share/gnome-shell/extensions/june@june.local"

# The window is a Tauri binary linked against the system WebKitGTK stack, which a fresh desktop often does not have. Checked before anything is installed, so a missing library reads as a message here instead of as a window that never opens.
missing="$(ldd "$here/june-window" 2>/dev/null | awk '/not found/ {print $1}' || true)"
if [ -n "$missing" ]; then
	echo "June's window needs shared libraries this machine does not have:" >&2
	echo "$missing" | sed 's/^/  /' >&2
	echo "On Debian or Ubuntu: sudo apt install libwebkit2gtk-4.1-0 libgtk-3-0 libsoup-3.0-0" >&2
	exit 1
fi

# Computer use (seeing and clicking the screen) is GNOME-only: the window frames it needs on Wayland come from the bundled GNOME Shell extension, and no other desktop loads a GNOME Shell extension at all. This is a degraded install, not a failed one — June still runs and can chat on any desktop — so it is reported rather than aborted, and the extension is not installed into a directory nothing on this desktop reads.
desktop="${XDG_CURRENT_DESKTOP:-unknown}"
case "$desktop" in
*GNOME*) is_gnome=true ;;
*) is_gnome=false ;;
esac
if [ "$is_gnome" = false ]; then
	echo "Desktop environment reports '$desktop', not GNOME: computer use needs GNOME Shell to load the window-raising extension, so June will not be able to see window positions or place clicks here. Chat and voice still work." >&2
fi

# Read before the copy: a first install turns start at login on, an upgrade keeps what the user has chosen in June's settings since.
upgrading=false
if [ -e "$bin/june" ]; then
	upgrading=true
fi

mkdir -p "$bin" "$apps"
install -m 0755 "$here/june" "$bin/june"
install -m 0755 "$here/june-window" "$bin/june-window"
# Exec= is rewritten to the absolute path of the installed binary. The entry ships saying `Exec=june`, and a desktop launching it that way finds nothing: ~/.local/bin only joins PATH when ~/.profile runs at login and the directory already exists, which on a machine installing June for the first time it did not.
sed "s|^Exec=june$|Exec=$bin/june|" "$here/june.desktop" > "$apps/june.desktop"
chmod 0644 "$apps/june.desktop"

if [ "$is_gnome" = true ]; then
	mkdir -p "$extdir"
	install -m 0644 "$here/gnome-extension/june@june.local/metadata.json" "$extdir/metadata.json"
	install -m 0644 "$here/gnome-extension/june@june.local/extension.js" "$extdir/extension.js"
fi

for size_dir in "$here"/icons/hicolor/*/apps; do
	size="$(basename "$(dirname "$size_dir")")"
	dest="$icons/$size/apps"
	mkdir -p "$dest"
	install -m 0644 "$size_dir/june.png" "$dest/june.png"
done

command -v update-desktop-database >/dev/null 2>&1 && update-desktop-database "$apps" || true
command -v gtk-update-icon-cache >/dev/null 2>&1 && gtk-update-icon-cache -f -t "$icons" || true

# Desktops that start login items through systemd (KDE Plasma and others using systemd-xdg-autostart-generator; GNOME starts them itself) run June's start-at-login entry as the generated unit app-june@autostart.service, which gives a program 5 seconds to stop at logout or shutdown before killing it. June's shutdown can take most of a minute filing the last stretch of activity into memory, so this gives it the 90 seconds the june.service of 0.1.0 had. A drop-in for a unit that never exists changes nothing, so it is written on every desktop.
dropin="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/app-june@autostart.service.d"
mkdir -p "$dropin"
cat > "$dropin/stop-timeout.conf" <<'EOF'
[Service]
TimeoutStopSec=90s
EOF

if [ "$is_gnome" = true ] && command -v gnome-extensions >/dev/null 2>&1; then
	# check-shell-version.sh reads gnome-shell's own version against metadata.json's shell-version list: outside that list gnome-shell silently refuses to load the extension, with no error dialog, so the version and the consequence are stated here rather than left to be discovered as window frames that never appear.
	if [ -x "$here/gnome-extension/check-shell-version.sh" ]; then
		"$here/gnome-extension/check-shell-version.sh" || true
	fi
	gnome-extensions enable june@june.local >/dev/null 2>&1 || true
	echo "Window-raising extension installed: on Wayland, log out and back in once to finish enabling it."
fi

# The service used to load ~/.config/june/env with EnvironmentFile, which put its keys in the daemon's environment ahead of <data dir>/env; the window's first-run setup saves keys to the latter, and godotenv never overrides a variable already set, so a key saved there would have been ignored. The old file's lines are merged in instead, still winning over the same key in the data dir's file, as they did before.
data="${JUNE_DATA_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/june}"
legacy_env="$HOME/.config/june/env"

# merge_env FILE DIR puts the lines of FILE into DIR/env, each key FILE sets replacing the same key there.
merge_env() {
	mkdir -p "$2"
	chmod 0700 "$2"
	merged="$(mktemp "$2/env.XXXXXX")"
	if [ -f "$2/env" ]; then
		keys="$(sed -n 's/^[[:space:]]*\(export[[:space:]][[:space:]]*\)\{0,1\}\([A-Za-z_][A-Za-z0-9_]*\)[[:space:]]*=.*/\2/p' "$1" | paste -sd'|' -)"
		if [ -n "$keys" ]; then
			grep -Ev "^[[:space:]]*(export[[:space:]]+)?($keys)[[:space:]]*=" "$2/env" > "$merged" || true
		else
			cat "$2/env" > "$merged"
		fi
		# A data file whose last line has no newline would otherwise run into the first merged line.
		echo >> "$merged"
	fi
	cat "$1" >> "$merged"
	chmod 0600 "$merged"
	mv "$merged" "$2/env"
}

if [ -f "$legacy_env" ]; then
	merge_env "$legacy_env" "$data"
	# The old file can also name another data folder, which the service handed June as JUNE_DATA_DIR. From the default folder's env file it still takes June there, but June reads keys only from that one file, while its setup saves new keys into the env file of the folder it was taken to, where nothing reads them. So the keys go there as well, and the user is told how to make June read that folder's own file.
	legacy_dir=""
	if [ -z "${JUNE_DATA_DIR:-}" ]; then
		legacy_dir="$(sed -n 's/^[[:space:]]*\(export[[:space:]][[:space:]]*\)\{0,1\}JUNE_DATA_DIR[[:space:]]*=[[:space:]]*//p' "$legacy_env" | tail -n 1 | sed -e 's/[[:space:]]*$//' -e 's/^"\(.*\)"$/\1/' -e "s/^'\(.*\)'$/\1/")"
	fi
	if [ -n "$legacy_dir" ] && [ "$legacy_dir" != "$data" ]; then
		keys_only="$(mktemp)"
		grep -Ev '^[[:space:]]*(export[[:space:]]+)?JUNE_DATA_DIR[[:space:]]*=' "$legacy_env" > "$keys_only" || true
		merge_env "$keys_only" "$legacy_dir"
		rm -f "$keys_only"
		echo "$legacy_env set JUNE_DATA_DIR=$legacy_dir. June still keeps its data there, but it reads keys only from $data/env, so a key saved later from June's window, which goes to $legacy_dir/env, would not be used. Set JUNE_DATA_DIR=$legacy_dir in your session instead (for example in ~/.config/environment.d/june.conf), then log out and back in." >&2
	fi
	rm -f "$legacy_env"
	rmdir "$HOME/.config/june" 2>/dev/null || true
	echo "Moved $legacy_env into $data/env"
fi

if [ -n "${JUNE_NO_SERVICE:-}" ] && [ "${JUNE_NO_SERVICE}" != 0 ]; then
	echo "June installed (JUNE_NO_SERVICE is set, so nothing was started and start at login was not changed; start it with: june --daemon)."
	exit 0
fi

# Up to 0.1.0 this script enabled a systemd user service to start June at login. June's own switch for that (in its settings, and `june --autostart`) writes an XDG autostart entry instead, which every desktop honours, while the service waited for graphical-session.target, which only some desktops reach. With both, two daemons raced for June's port at every login, and setup showed start at login as off while June started anyway. So the service is retired, and a login start it gave is carried over to June's switch.
had_service=false
if [ -f "$units/june.service" ] && command -v systemctl >/dev/null 2>&1; then
	if systemctl --user is-enabled --quiet june.service 2>/dev/null; then
		had_service=true
	fi
	systemctl --user disable --now june.service >/dev/null 2>&1 || true
	rm -f "$units/june.service"
fi
# Also loads the stop-timeout drop-in above into a user manager that is already running.
if command -v systemctl >/dev/null 2>&1; then
	systemctl --user daemon-reload >/dev/null 2>&1 || true
fi

# The daemon started below, and every daemon it later restarts into, keeps this script's working directory, which can be a temporary folder the download step deletes as soon as this script ends.
cd "$HOME"

if [ "$upgrading" = false ] || [ "$had_service" = true ]; then
	"$bin/june" --autostart on
fi

echo "Run 'june doctor' at any time to see what June can use on this machine."
# Over SSH or from a text console there is no desktop to show June on, and a daemon started from here would run without one for the rest of the session. A June already running in the user's desktop is left running too: stopped from here, it would stay gone until they opened it again.
if [ -z "${WAYLAND_DISPLAY:-}" ] && [ -z "${DISPLAY:-}" ]; then
	if [ "$had_service" = true ]; then
		echo "June installed. The login service earlier versions used was removed, and with it any June it was running. This terminal is not in a desktop session, so June was not opened from it: it starts again at your next desktop login, or open it from the app menu there."
	else
		echo "June installed. This terminal is not in a desktop session, so June was not opened from it. A June already running keeps the old version until it quits: quit it from June's tray menu (or run june --quit), then open June from the app menu in the desktop to start the new one."
	fi
	exit 0
fi

# A June still running from before keeps the old binary until it exits, and opening June below would only show that one. --quit asks whichever June answers on June's port, and succeeds at once when none does. Its failure is not the install's: one still finishing its shutdown is followed by the June that opening starts, and one too old to be asked is named by june itself, with what to do.
"$bin/june" --quit || true
echo "June installed. Opening June, which walks you through the rest of setup."
# June's own report is the answer, not the install's: an install that copied every file worked even if the display turns out to be unreachable. It starts the daemon in a session of its own, so closing this terminal does not end June.
"$bin/june" || true
