#!/usr/bin/env bash
# Fails if the staged release directory named in $1 is missing a file the downloadable package must ship, or has one that is not executable when it should be. This is the regression test for packaging/release.sh's file list: run it against a directory of empty placeholder files to check the list without doing a full build.
set -uo pipefail
dist="${1:?usage: check-stage.sh <staged-directory>}"
missing=0

require() {
	if [ ! -e "$dist/$1" ]; then
		echo "missing $1" >&2
		missing=1
	fi
}

require_exec() {
	require "$1"
	if [ -e "$dist/$1" ] && [ ! -x "$dist/$1" ]; then
		echo "$1 is not executable" >&2
		missing=1
	fi
}

require_exec june
require_exec june-window
require_exec install.sh
require_exec uninstall.sh
require june.desktop
require "gnome-extension/june@june.local/metadata.json"
require "gnome-extension/june@june.local/extension.js"
# install.sh runs this before enabling the extension, and only when it is shipped executable.
require_exec "gnome-extension/check-shell-version.sh"

for size in 16 32 48 64 128 256; do
	require "icons/hicolor/${size}x${size}/apps/june.png"
done

exit $missing
