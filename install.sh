#!/usr/bin/env bash
# Downloads the latest June release for this machine, checks it against the release's SHA256SUMS, and runs the installer inside it. Usage: curl -fsSL https://raw.githubusercontent.com/M-DEV-1/june/main/install.sh | bash
set -euo pipefail

repo="M-DEV-1/june"

# Git Bash and WSL users on Windows reach this script too; Git Bash would fetch a Linux build that cannot run there.
case "$(uname -s)" in
Linux) ;;
MINGW* | MSYS* | CYGWIN*)
	echo "This installer is for Linux. On Windows, download https://github.com/$repo/releases/latest/download/June-Setup-x64.exe, or in PowerShell run: irm https://raw.githubusercontent.com/$repo/main/install.ps1 | iex" >&2
	exit 1
	;;
*)
	echo "June has no build for $(uname -s) yet; it runs on Linux and Windows." >&2
	exit 1
	;;
esac

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
