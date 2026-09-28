#!/usr/bin/env bash
# Downloads the latest June release for this machine, checks it against the release's SHA256SUMS, and runs the installer inside it. Usage: curl -fsSL https://raw.githubusercontent.com/M-DEV-1/june/main/install.sh | bash
set -euo pipefail

repo="M-DEV-1/june"
archive="june-linux-$(uname -m).tar.gz"
base="https://github.com/$repo/releases/latest/download"

for cmd in curl tar sha256sum; do
	command -v "$cmd" >/dev/null 2>&1 || { echo "June's installer needs $cmd" >&2; exit 1; }
done

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
cd "$tmp"

echo "downloading $archive..."
if ! curl -fL --progress-bar -o "$archive" "$base/$archive"; then
	echo "no June release for $(uname -m) at $base/$archive" >&2
	exit 1
fi
curl -fsSL -o SHA256SUMS "$base/SHA256SUMS"
sha256sum --check --ignore-missing --quiet SHA256SUMS

tar -xzf "$archive"
dir="$(find . -mindepth 1 -maxdepth 1 -type d -name 'june-*' | head -1)"
"$dir/install.sh"
