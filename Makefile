# Builds the daemon and the desktop window in release mode and stages a downloadable package under dist/. See scripts/release.sh for the actual steps.
.PHONY: release clean

release:
	./scripts/release.sh

clean:
	rm -rf dist
