package update

import (
	"cmp"
	"strconv"
	"strings"
)

// version is a release number as semver orders it.
type version struct {
	core [3]int
	pre  []string
}

// parseVersion reads a release number such as "0.2.1", "v0.2.1" or "0.2.0-rc.1". Build metadata after a "+" is dropped, since semver gives it no order. Output: the version, and false for anything else, "dev" included, which is what keeps a build without a release number from ever updating itself.
// Only what semver allows gets through, with no space or path character anywhere, because a release's version also names the installer's file in <data>/update.
func parseVersion(s string) (version, bool) {
	s = strings.TrimPrefix(s, "v")
	s, build, hasBuild := strings.Cut(s, "+")
	if hasBuild && !identifiers(build) {
		return version{}, false
	}
	core, pre, hasPre := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return version{}, false
	}
	var v version
	for i, p := range parts {
		n, ok := number(p)
		if !ok {
			return version{}, false
		}
		v.core[i] = n
	}
	if hasPre {
		if !identifiers(pre) {
			return version{}, false
		}
		v.pre = strings.Split(pre, ".")
	}
	return v, true
}

// identifiers reports whether s is dot-separated identifiers as semver writes a pre-release or build: none empty, and nothing but ASCII letters, digits and hyphens in any.
func identifiers(s string) bool {
	for id := range strings.SplitSeq(s, ".") {
		if id == "" || strings.Trim(id, "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-") != "" {
			return false
		}
	}
	return true
}

// number reads s as a non-negative decimal with nothing else in it; strconv.Atoi alone would also take "+1" and "-1".
func number(s string) (int, bool) {
	if s == "" || strings.Trim(s, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// compare orders two versions the semver way: a pre-release sorts before the release it leads up to, so a tester on 0.2.0-rc.1 is offered 0.2.0. Output: -1, 0 or 1.
func (a version) compare(b version) int {
	for i := range a.core {
		if c := cmp.Compare(a.core[i], b.core[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(a.pre) == 0 && len(b.pre) == 0:
		return 0
	case len(a.pre) == 0:
		return 1
	case len(b.pre) == 0:
		return -1
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		an, aNum := number(a.pre[i])
		bn, bNum := number(b.pre[i])
		var c int
		switch {
		case aNum && bNum:
			c = cmp.Compare(an, bn)
		case aNum:
			c = -1
		case bNum:
			c = 1
		default:
			c = strings.Compare(a.pre[i], b.pre[i])
		}
		if c != 0 {
			return c
		}
	}
	return cmp.Compare(len(a.pre), len(b.pre))
}
