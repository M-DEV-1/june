#!/usr/bin/env bash
# Sets June's version in every file that carries it (tauri.conf.json, Cargo.toml, Cargo.lock and package.json) and then runs check-version.sh, so the tree is ready to tag v<version>. sed runs with -b because Git for Windows' sed otherwise rewrites a CRLF checkout's files with LF endings.
# Usage: packaging/bump-version.sh 0.2.1
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
new="${1:?usage: bump-version.sh <version, e.g. 0.2.1>}"
new="${new#v}"

if ! [[ "$new" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
	echo "'$new' is not X.Y.Z or X.Y.Z-prerelease" >&2
	exit 1
fi

cd "$root"
# Only the top-level "version" key, which both JSON files indent by exactly two spaces; nothing nested matches.
sed -b -i -E "s/^(  \"version\": *\")[^\"]*(\")/\1$new\2/" app/src-tauri/tauri.conf.json app/package.json
# The first version = line is [package]'s; dependencies give theirs inline, never at the start of a line.
sed -b -i -E "0,/^version = \"[^\"]*\"/s//version = \"$new\"/" app/src-tauri/Cargo.toml
# June's crate in the lock file is the entry whose name line is exactly "june"; its version is the next line.
sed -b -i -E "/^name = \"june\"\r?$/{n;s/^version = \"[^\"]*\"/version = \"$new\"/;}" app/src-tauri/Cargo.lock

bash "$root/packaging/check-version.sh" "v$new"
