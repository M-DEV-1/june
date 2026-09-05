#!/usr/bin/env bash
# Copies this extension into the user's GNOME Shell extensions directory and enables it. On Wayland a brand new extension is loaded by the shell's own directory watcher; if it does not appear, log out and back in.
set -euo pipefail

src="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
dest="$HOME/.local/share/gnome-shell/extensions/ora@local"

mkdir -p "$dest"
cp "$src/metadata.json" "$src/extension.js" "$src/stylesheet.css" "$dest/"
echo "installed to $dest"

gnome-extensions enable ora@local || echo "enable failed; log out and back in, then run: gnome-extensions enable ora@local"
gnome-extensions info ora@local || true
