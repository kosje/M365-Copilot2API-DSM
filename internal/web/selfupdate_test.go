package web

import "testing"

func TestSemverAtLeast(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"v1.3.4", "1.3.3", true},
		{"v2.0.0", "1.9.9", true},
		{"v1.4.0", "1.4.0", false},
		{"v1.3.3", "1.3.4", false},
		{"v1.10.0", "1.9.0", true},  // numeric, not lexicographic
		{"v1.3.10", "1.3.9", true},  // numeric, not lexicographic
		{"1.4.0", "v1.3.9", true},   // optional v prefix on either side
		{"v1.4", "1.3.9", true},     // partial semver
		{"v1.4.0-beta", "1.3.9", true},
	}
	for _, c := range cases {
		if got := semverAtLeast(c.latest, c.current); got != c.want {
			t.Errorf("semverAtLeast(%q, %q) = %v, want %v", c.latest, c.current, got, c.want)
		}
	}
}
