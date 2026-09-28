#!/usr/bin/env bash
# Checks whether the running GNOME Shell's major version is one this extension declares support for in metadata.json. A mismatch here is the most common reason "installed but not answering" happens: gnome-shell silently refuses to load an extension whose shell-version list does not name it, with no error dialog. Input: none (reads gnome-shell --version and the metadata.json next to this script). Output: a one-line verdict on stdout; exit 0 if covered, exit 1 if not or if the version could not be determined.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
metadata="$here/june@june.local/metadata.json"

detect_major() {
	if command -v gnome-shell >/dev/null 2>&1; then
		gnome-shell --version | grep -oE '[0-9]+' | head -1
		return
	fi
	# No gnome-shell binary on PATH (some distros keep it out of PATH): ask the shell itself over D-Bus.
	if command -v gdbus >/dev/null 2>&1; then
		gdbus call --session --dest org.gnome.Shell --object-path /org/gnome/Shell \
			--method org.freedesktop.DBus.Properties.Get org.gnome.Shell ShellVersion 2>/dev/null \
			| grep -oE '[0-9]+' | head -1
	fi
}

covered() {
	local major="$1"
	grep -oE '"[0-9]+"' "$metadata" | tr -d '"' | grep -qx "$major"
}

major="$(detect_major || true)"
if [ -z "${major:-}" ]; then
	echo "could not determine the running GNOME Shell version; install anyway and check with: gnome-extensions info june@june.local"
	exit 1
fi

if covered "$major"; then
	echo "GNOME Shell $major is in metadata.json's shell-version list: the extension will load once enabled."
	exit 0
fi

echo "GNOME Shell $major is NOT in metadata.json's shell-version list ($(grep -oE '"shell-version".*' "$metadata")): gnome-shell will silently refuse to load it. Add \"$major\" to shell-version in $metadata if you have checked this extension still works (it only uses stable Gio/GLib D-Bus calls, no deprecated widget APIs), then reinstall."
exit 1
