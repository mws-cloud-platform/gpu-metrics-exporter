package selfupdate

import "testing"

func TestParseVersion(t *testing.T) {
	for _, s := range []string{"2026.10.06-1", "2026.07.29-12", "2026.10.06-1-3-gabc1234"} {
		if _, err := ParseVersion(s); err != nil {
			t.Errorf("ParseVersion(%q): %v", s, err)
		}
	}
	// What a plain `go build`, `git describe --always` without a tag, and
	// build-deb.sh's fallback produce, plus near misses.
	for _, s := range []string{"", "abc1234", "0.0.0-dev", "2026.10.06", "2026.10.6-1", "v2026.10.06-1",
		"2026.10.06-1-dirty", "2026.10.06-1-3", "2026.10.06-1-3-gXYZ", " 2026.10.06-1", "2026.10.06-99999999999999999999"} {
		if v, err := ParseVersion(s); err == nil {
			t.Errorf("ParseVersion(%q) = %v, want an error", s, v)
		}
	}
}

func TestVersionCompare(t *testing.T) {
	// Each is newer than the one before it.
	ordered := []string{
		"2026.07.29-1",
		"2026.07.29-2",
		"2026.07.29-10", // numerically, not as text
		"2026.07.29-10-1-g0000000",
		"2026.07.29-10-12-gfffffff",
		"2026.09.01-1",
		"2026.10.06-1",
		"2027.01.01-1",
	}
	for i := range ordered {
		for j := range ordered {
			a, b := mustParse(t, ordered[i]), mustParse(t, ordered[j])
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got := a.Compare(b); got != want {
				t.Errorf("%s.Compare(%s) = %d, want %d", a, b, got, want)
			}
		}
	}
	if !mustParse(t, "2026.10.06-1").IsRelease() || mustParse(t, "2026.10.06-1-3-gabc1234").IsRelease() {
		t.Error("IsRelease tells a release from a build past one wrong")
	}
}

func mustParse(t *testing.T, s string) Version {
	t.Helper()
	v, err := ParseVersion(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
