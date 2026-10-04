#!/usr/bin/env bash
# Fails unless every file that carries June's version agrees: app/src-tauri/tauri.conf.json (the source of truth), app/src-tauri/Cargo.toml, June's own entry in app/src-tauri/Cargo.lock, and app/package.json. Given a tag (v0.2.0 or 0.2.0), the version must also equal it, so a release is never built from a tree that reports another number.
# Prints the version, and on GitHub Actions also writes version=, numeric= (the four-part number Windows file metadata needs) and prerelease= to $GITHUB_OUTPUT.
# The Go programs carry no version file of their own: the release builds stamp config.Version with -ldflags -X from this same number.
# Usage: packaging/check-version.sh [tag]
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Every read strips carriage returns, since a Windows checkout has these files with CRLF line endings.
read_version() {
	tr -d '\r' < "$root/$1" | sed -n "$2" | head -1
}

tauri="$(read_version app/src-tauri/tauri.conf.json 's/^  "version": *"\([^"]*\)".*/\1/p')"
cargo="$(read_version app/src-tauri/Cargo.toml 's/^version = "\([^"]*\)".*/\1/p')"
# The lock file names June's crate on one line and its version on the next.
lock="$(tr -d '\r' < "$root/app/src-tauri/Cargo.lock" | sed -n '/^name = "june"$/{n;s/^version = "\([^"]*\)"$/\1/p;}' | head -1)"
npm="$(read_version app/package.json 's/^  "version": *"\([^"]*\)".*/\1/p')"

fail=0
if [ -z "$tauri" ]; then
	echo "could not read a version from app/src-tauri/tauri.conf.json" >&2
	exit 1
fi
# A plain X.Y.Z with an optional -prerelease: Windows file metadata needs the three numbers, and a +build suffix would end up in file names.
if ! [[ "$tauri" =~ ^([0-9]+)\.([0-9]+)\.([0-9]+)(-[0-9A-Za-z.-]+)?$ ]]; then
	echo "tauri.conf.json's version '$tauri' is not X.Y.Z or X.Y.Z-prerelease" >&2
	exit 1
fi
for part in "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}" "${BASH_REMATCH[3]}"; do
	# Each part of a Windows file version is a 16-bit number.
	if [ "$part" -gt 65535 ]; then
		echo "version part $part is larger than the 65535 a Windows file version can hold" >&2
		exit 1
	fi
done
numeric="${BASH_REMATCH[1]}.${BASH_REMATCH[2]}.${BASH_REMATCH[3]}.0"
prerelease=false
[ -n "${BASH_REMATCH[4]}" ] && prerelease=true

check() {
	if [ "$2" != "$tauri" ]; then
		echo "$1 says '$2' but tauri.conf.json says '$tauri'" >&2
		fail=1
	fi
}
check app/src-tauri/Cargo.toml "$cargo"
check "app/src-tauri/Cargo.lock (package june)" "$lock"
check app/package.json "$npm"

if [ $# -gt 0 ] && [ -n "$1" ]; then
	tag="${1#refs/tags/}"
	if [ "${tag#v}" != "$tauri" ]; then
		echo "tag '$1' does not match the version '$tauri' in the tree; run packaging/bump-version.sh ${tag#v} and commit before tagging" >&2
		fail=1
	fi
fi

if [ $fail -ne 0 ]; then
	exit 1
fi

echo "$tauri"
if [ -n "${GITHUB_OUTPUT:-}" ]; then
	{
		echo "version=$tauri"
		echo "numeric=$numeric"
		echo "prerelease=$prerelease"
	} >> "$GITHUB_OUTPUT"
fi
