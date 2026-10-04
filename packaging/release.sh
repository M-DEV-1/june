#!/usr/bin/env bash
# Builds the Go daemon and the Tauri desktop window in release mode and stages a downloadable package under dist/june-<version>/. Never installs anything: that is packaging/install.sh's job, run by hand afterwards against the staged directory.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

# check-version.sh fails unless tauri.conf.json, Cargo.toml, Cargo.lock and package.json agree, and prints the version they agree on.
version="$(bash packaging/check-version.sh)"
# VERSION (the release workflow passes the tag's) must be the tree's: the window takes its version from tauri.conf.json at compile time, so building any other number would ship two programs that disagree about which June they are.
if [ -n "${VERSION:-}" ] && [ "${VERSION#v}" != "$version" ]; then
	echo "VERSION is $VERSION but the tree says $version; run packaging/bump-version.sh ${VERSION#v} first" >&2
	exit 1
fi
dist="$root/dist/june-$version"

echo "staging into $dist"
rm -rf "$dist"
mkdir -p "$dist"

echo "building the daemon..."
# -X stamps the version GET /settings, GET /setup and the updater report; a build without it calls itself "dev". -trimpath keeps this machine's paths out of the binary.
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X june/internal/config.Version=$version" -o "$dist/june" .

echo "building the desktop window..."
if [ ! -d app/node_modules ]; then
	( cd app && pnpm install --frozen-lockfile )
fi
( cd app && pnpm build )
( cd app/src-tauri && cargo build --release --locked )
install -m 0755 app/src-tauri/target/release/june "$dist/june-window"

echo "generating icons..."
for size in 16 32 48 64 128 256; do
	dir="$dist/icons/hicolor/${size}x${size}/apps"
	mkdir -p "$dir"
	convert app/src-tauri/icons/icon.png -resize "${size}x${size}" "$dir/june.png"
done

cp packaging/june.desktop "$dist/june.desktop"
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
