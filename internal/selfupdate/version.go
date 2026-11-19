package selfupdate

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
)

// Release tags, and with them the package and binary versions, look like
// 2026.10.06-1: a date and that day's build number. Builds from a branch are
// versioned by `git describe --tags --always` (Makefile, build-deb.sh), which
// appends how far past the tag they are: 2026.10.06-1-3-gabc1234.
var versionRE = regexp.MustCompile(`^([0-9]{4})\.([0-9]{2})\.([0-9]{2})-([0-9]+)(?:-([0-9]+)-g[0-9a-f]+)?$`)

// Version is a parsed exporter version.
type Version struct {
	// year, month, day, build number, commits past the tag (0 for a release)
	fields [5]int
	raw    string
}

// ParseVersion parses a release version or a `git describe` build of one.
// Anything else -- an empty version from a plain `go build`, a bare commit
// hash, build-deb.sh's 0.0.0-dev -- is an error: there is no telling how it
// orders against a release.
func ParseVersion(s string) (Version, error) {
	m := versionRE.FindStringSubmatch(s)
	if m == nil {
		return Version{}, fmt.Errorf("%q is neither a release version (YYYY.MM.DD-N) nor a build from one", s)
	}
	v := Version{raw: s}
	for i, digits := range m[1:] {
		if digits == "" {
			continue // a release: no commits past the tag
		}
		n, err := strconv.Atoi(digits)
		if err != nil {
			return Version{}, fmt.Errorf("version %q: %w", s, err)
		}
		v.fields[i] = n
	}
	return v, nil
}

// IsRelease reports a tagged release rather than a build past one.
func (v Version) IsRelease() bool { return v.fields[4] == 0 }

func (v Version) String() string { return v.raw }

// Compare returns -1, 0 or +1 as v is older than, the same as, or newer than
// o. A build past a tag orders after that tag and before the next release.
func (v Version) Compare(o Version) int { return slices.Compare(v.fields[:], o.fields[:]) }
