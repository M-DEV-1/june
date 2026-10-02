#!/bin/sh
# Fails when Go code is dead on both Linux and Windows. A function or type used on only one platform shows as dead in the other platform's run, so only what both runs report counts. Tests are not entry points: a function only a test calls is a seam, and seams belong in the package's _test.go files.
# Allowed: the router's two test seams, which brain's tests in another package need, and the shared test-store package.
set -eu
allow='internal/agent/router.go (ResetRouter|SetProviderReady)$|^internal/db/dbtest/'
tmp=$(mktemp -d)
for os in linux windows; do
	GOOS=$os deadcode ./... | sed -E 's/:[0-9]+:[0-9]+: unreachable func: / /' | sort > "$tmp/deadcode-$os"
	GOOS=$os staticcheck -checks U1000 ./... | sed -E 's/:[0-9]+:[0-9]+:/:/' | sort > "$tmp/unused-$os" || true
done
dead=$( (comm -12 "$tmp/deadcode-linux" "$tmp/deadcode-windows"; comm -12 "$tmp/unused-linux" "$tmp/unused-windows") | grep -Ev "$allow" || true)
if [ -n "$dead" ]; then
	echo "dead on both Linux and Windows:"
	echo "$dead"
	exit 1
fi
