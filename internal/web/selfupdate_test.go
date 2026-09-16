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
		{"v1.10.0", "1.9.0", true}, // numeric, not lexicographic
		{"v1.3.10", "1.3.9", true}, // numeric, not lexicographic
		{"1.4.0", "v1.3.9", true},  // optional v prefix on either side
		{"v1.4", "1.3.9", true},    // partial semver
		{"v1.4.0-beta", "1.3.9", true},

		// Fourth component: the DSM build numbers packaging-only revisions as
		// 1.5.2.1 on top of upstream 1.5.2. Comparison must not stop at three
		// components, or such a release looks identical to its base and the
		// console banner never offers it.
		{"v1.5.2.1", "1.5.2", true},
		{"v1.5.2.2", "1.5.2.1", true},
		{"v1.5.2.1", "1.5.2.1", false},
		{"v1.5.2", "1.5.2.1", false}, // base tag must not "update" a revision
		{"v1.5.3", "1.5.2.9", true},  // an upstream bump still wins
	}
	for _, c := range cases {
		if got := semverAtLeast(c.latest, c.current); got != c.want {
			t.Errorf("semverAtLeast(%q, %q) = %v, want %v", c.latest, c.current, got, c.want)
		}
	}
}
