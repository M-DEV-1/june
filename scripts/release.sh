#!/usr/bin/env bash
# Builds the Go daemon and the Tauri desktop window in release mode and stages a downloadable package under dist/ora-<version>/. Never installs anything: that is packaging/install.sh's job, run by hand afterwards against the staged directory.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

version="$(sed -n 's/.*"version": *"\([^"]*\)".*/\1/p' app/src-tauri/tauri.conf.json | head -1)"
if [ -z "$version" ]; then
	echo "could not read a version from app/src-tauri/tauri.conf.json" >&2
	exit 1
fi
dist="$root/dist/ora-$version"

echo "staging into $dist"
rm -rf "$dist"
mkdir -p "$dist"

echo "building the daemon..."
CGO_ENABLED=0 go build -o "$dist/ora" .

echo "building the desktop window..."
if [ ! -d app/node_modules ]; then
	( cd app && pnpm install --frozen-lockfile )
fi
( cd app && pnpm build )
( cd app/src-tauri && cargo build --release )
install -m 0755 app/src-tauri/target/release/ora "$dist/ora-window"

echo "generating icons..."
for size in 16 32 48 64 128 256; do
	dir="$dist/icons/hicolor/${size}x${size}/apps"
	mkdir -p "$dir"
	convert app/src-tauri/icons/icon.png -resize "${size}x${size}" "$dir/ora.png"
done

cp packaging/ora.desktop "$dist/ora.desktop"
cp packaging/ora.service "$dist/ora.service"
install -m 0755 packaging/install.sh "$dist/install.sh"
install -m 0755 packaging/uninstall.sh "$dist/uninstall.sh"
mkdir -p "$dist/gnome-extension/ora@ora.local"
cp packaging/gnome-extension/ora@ora.local/metadata.json packaging/gnome-extension/ora@ora.local/extension.js "$dist/gnome-extension/ora@ora.local/"

"$root/scripts/check-stage.sh" "$dist"

# The thing a person downloads is one file, not a directory, and the tar has to carry the executable bits install.sh relies on.
echo "packing the archive..."
tar -czf "$root/dist/ora-$version.tar.gz" -C "$root/dist" "ora-$version"

echo "staged: $dist"
echo "archive: $root/dist/ora-$version.tar.gz"
du -ah "$dist" | sort -k2
