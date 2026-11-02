package cli

import (
	"cmp"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// tagVersion is a tag read as a version: [v]N[.N[.N]][-suffix], such as 24-alpine, v1.3 or 1.4.0-rc1-bookworm.
// A suffix that begins with a prerelease marker, alpha, beta, rc or pre, with an optional "." and number, makes it a
// prerelease, and the rest of the suffix, after a "-", is its variant: 1.4-rc1-alpine is a prerelease of 1.4,
// variant alpine. Tags compare only with tags of the same v prefix and variant, which make up their kind.
type tagVersion struct {
	tag     string
	v       bool   // whether it starts with "v"
	numbers []int  // one to three
	pre     string // the prerelease marker, such as "rc1", or "" for a stable release
	variant string // the rest of the suffix, such as "alpine", or ""
}

// versionTag matches a tag that reads as a version: its v, its numbers, and its suffix.
var versionTag = regexp.MustCompile(`^(v?)(\d+(?:\.\d+){0,2})(?:-(.+))?$`)

// prereleaseMarker matches a suffix that marks a prerelease: the marker, and the variant after it.
var prereleaseMarker = regexp.MustCompile(`^((?:alpha|beta|rc|pre)(?:\.?\d+)?)(?:-(.+))?$`)

// parseVersion reads tag as a version, or reports that it isn't one, as latest and bookworm aren't.
func parseVersion(tag string) (tagVersion, bool) {
	m := versionTag.FindStringSubmatch(tag)
	if m == nil {
		return tagVersion{}, false
	}
	v := tagVersion{tag: tag, v: m[1] == "v", variant: m[3]}
	for _, n := range strings.Split(m[2], ".") {
		i, err := strconv.Atoi(n)
		if err != nil {
			return tagVersion{}, false // too large to compare
		}
		v.numbers = append(v.numbers, i)
	}
	if p := prereleaseMarker.FindStringSubmatch(v.variant); p != nil {
		v.pre, v.variant = p[1], p[2]
	}
	return v, true
}

// sameKind reports whether v and o compare: with the same v prefix and variant.
func (v tagVersion) sameKind(o tagVersion) bool {
	return v.v == o.v && v.variant == o.variant
}

// covers reports whether v, a stable release, covers o: whether o's leading numbers are v's,
// as 1.3.2's are 1.3's, and 1.30's aren't, and it has as many numbers or more, so that coarser tags are ignored.
func (v tagVersion) covers(o tagVersion) bool {
	return v.sameKind(o) && len(o.numbers) >= len(v.numbers) && slices.Equal(o.numbers[:len(v.numbers)], v.numbers)
}

// compare orders v and o, of the same kind, oldest first: by their numbers, a missing one as 0, then a prerelease
// before its release, prereleases by marker, then the tag with more numbers, the finer, as newer.
func (v tagVersion) compare(o tagVersion) int {
	for i := range max(len(v.numbers), len(o.numbers)) {
		if c := cmp.Compare(number(v.numbers, i), number(o.numbers, i)); c != 0 {
			return c
		}
	}
	switch {
	case v.pre == "" && o.pre != "":
		return 1
	case v.pre != "" && o.pre == "":
		return -1
	}
	if c := comparePre(v.pre, o.pre); c != 0 {
		return c
	}
	return cmp.Compare(len(v.numbers), len(o.numbers))
}

// number returns numbers[i], or 0 past its end.
func number(numbers []int, i int) int {
	if i < len(numbers) {
		return numbers[i]
	}
	return 0
}

// comparePre orders prerelease markers: alpha, beta, pre and rc, in that order, then by their numbers, compared as
// decimals of any length.
func comparePre(a, b string) int {
	split := func(s string) (string, string) {
		name := strings.TrimRight(s, "0123456789.")
		return name, strings.TrimLeft(strings.TrimPrefix(s[len(name):], "."), "0")
	}
	an, ad := split(a)
	bn, bd := split(b)
	return cmp.Or(cmp.Compare(an, bn), cmp.Compare(len(ad), len(bd)), cmp.Compare(ad, bd))
}

// newerThan reports whether v is newer than best, of the same kind, or best is none, the zero tagVersion:
// tags that compare equal, as 24.04 and 24.4 do, are told apart by name, so that the order the registry lists them
// in doesn't matter.
func (v tagVersion) newerThan(best tagVersion) bool {
	if best.tag == "" {
		return true
	}
	c := v.compare(best)
	return c > 0 || c == 0 && v.tag > best.tag
}

// resolveTag returns the tag that pulling tag resolves to, among listed, the tags its repository lists:
// the newest stable tag that it covers, if it is a stable version, or else tag itself.
func resolveTag(tag string, listed []string) string {
	pulled, ok := parseVersion(tag)
	if !ok || pulled.pre != "" {
		return tag
	}
	var resolved tagVersion
	for _, t := range listed {
		v, ok := parseVersion(t)
		if ok && v.pre == "" && pulled.covers(v) && v.newerThan(resolved) {
			resolved = v
		}
	}
	if resolved.tag == "" {
		return tag
	}
	return resolved.tag
}

// newer is what updating reports of a pulled tag, of the tags its repository lists: the newest stable tag of a
// newer minor or patch release within its major version, or the final release of a prerelease, and of a newer major
// version, each "" where there is none, and the newest prerelease newer than the tag it resolved to, or "" for none;
// or, for a tag that isn't a version, that it isn't.
type newer struct {
	unlisted   error // why the repository's tags couldn't be listed, so that the tag resolved to itself
	notVersion bool
	minor      string // a newer release within the major version, as minorKind says
	minorKind  string // "minor", "patch", or "final" for the release of the prerelease pulled
	major      string
	prerelease string
}

// newerReleases returns what updating reports of tag, which resolved to resolved, among listed: tags of the same
// kind only, of which a tag that tag covers, which resolving would choose, isn't newer.
func newerReleases(tag, resolved string, listed []string) newer {
	pulled, ok := parseVersion(tag)
	if !ok {
		return newer{notVersion: true}
	}
	current, ok := parseVersion(resolved)
	if !ok {
		current = pulled
	}
	var minor, major, pre tagVersion
	for _, t := range listed {
		v, ok := parseVersion(t)
		if !ok || !v.sameKind(pulled) || v.compare(current) <= 0 {
			continue
		}
		best := &major
		switch {
		case v.pre != "":
			best = &pre
		case pulled.pre == "" && pulled.covers(v):
			continue
		case v.numbers[0] == pulled.numbers[0]:
			best = &minor
		}
		if v.newerThan(*best) {
			*best = v
		}
	}
	n := newer{minor: minor.tag, major: major.tag, prerelease: pre.tag}
	switch {
	case minor.tag == "":
	case number(minor.numbers, 1) != number(current.numbers, 1):
		n.minorKind = "minor"
	case number(minor.numbers, 2) != number(current.numbers, 2):
		n.minorKind = "patch"
	default:
		n.minorKind = "final"
	}
	return n
}
