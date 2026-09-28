# Builds the daemon and the desktop window in release mode and stages a downloadable package under dist/. See packaging/release.sh for the actual steps.
.PHONY: release clean

release:
	./packaging/release.sh

clean:
	rm -rf dist
