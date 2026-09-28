#!/usr/bin/env bash
# Builds the Go daemon and the Tauri desktop window in release mode and stages a downloadable package under dist/june-<version>/. Never installs anything: that is packaging/install.sh's job, run by hand afterwards against the staged directory.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

version="$(sed -n 's/.*"version": *"\([^"]*\)".*/\1/p' app/src-tauri/tauri.conf.json | head -1)"
if [ -z "$version" ]; then
	echo "could not read a version from app/src-tauri/tauri.conf.json" >&2
	exit 1
fi
dist="$root/dist/june-$version"

echo "staging into $dist"
rm -rf "$dist"
mkdir -p "$dist"

echo "building the daemon..."
CGO_ENABLED=0 go build -o "$dist/june" .

echo "building the desktop window..."
if [ ! -d app/node_modules ]; then
	( cd app && pnpm install --frozen-lockfile )
fi
( cd app && pnpm build )
( cd app/src-tauri && cargo build --release )
install -m 0755 app/src-tauri/target/release/june "$dist/june-window"

echo "generating icons..."
for size in 16 32 48 64 128 256; do
	dir="$dist/icons/hicolor/${size}x${size}/apps"
	mkdir -p "$dir"
	convert app/src-tauri/icons/icon.png -resize "${size}x${size}" "$dir/june.png"
done

cp packaging/june.desktop "$dist/june.desktop"
cp packaging/june.service "$dist/june.service"
install -m 0755 packaging/install.sh "$dist/install.sh"
install -m 0755 packaging/uninstall.sh "$dist/uninstall.sh"
mkdir -p "$dist/gnome-extension/june@june.local"
cp packaging/gnome-extension/june@june.local/metadata.json packaging/gnome-extension/june@june.local/extension.js "$dist/gnome-extension/june@june.local/"
install -m 0755 packaging/gnome-extension/check-shell-version.sh "$dist/gnome-extension/check-shell-version.sh"

"$root/packaging/check-stage.sh" "$dist"

# The thing a person downloads is one file, not a directory, and the tar has to carry the executable bits install.sh relies on.
echo "packing the archive..."
# The name carries no version, so releases/latest/download/<name> is a link that never changes; the root install.sh fetches exactly that name.
archive="june-linux-$(uname -m).tar.gz"
tar -czf "$root/dist/$archive" -C "$root/dist" "june-$version"
( cd "$root/dist" && sha256sum "$archive" > SHA256SUMS )

echo "staged: $dist"
echo "archive: $root/dist/$archive"
du -ah "$dist" | sort -k2
