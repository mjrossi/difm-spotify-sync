package match_test

import (
	"math"
	"strings"
	"testing"

	"github.com/mjrossi/difm-spotify-sync/pkg/match"
)

// Seeds drawn from the table tests plus the shapes a decoder-defensive
// client can still hand this package: empty, whitespace, unbalanced
// brackets, mixed scripts, and a title that is nothing but markers.
var fuzzSeeds = [][2]string{
	{"Funk D'Void & Berny", "Junkies (Joe Silva Remix)"},
	{"DJ Rax", "Air Race (Spiritchaser Remix)"},
	{"Elements Of Life", "Live Your Life For Today"},
	{"Various Artists", "!!!"},
	{"", ""},
	{"   ", "\t\n"},
	{"A", "("},
	{"Björk feat. 坂本龍一", "Ærø – Radio Edit [Extended Mix] (feat. Ωmega)"},
	{"x", "(Remix) (Radio Edit) [Extended] feat."},
	{"long", strings.Repeat("a very long title ", 600)},
}

// The matcher normalizes whatever the two APIs send. A panic here takes
// the daemon down on every tick that re-reads the same like, so the
// property is simply: never.
func FuzzNormalize(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s[0])
		f.Add(s[1])
	}
	f.Fuzz(func(t *testing.T, s string) {
		once := match.Normalize(s)
		if twice := match.Normalize(once); twice != once {
			t.Errorf("Normalize is not idempotent: %q -> %q -> %q", s, once, twice)
		}
	})
}

func FuzzParse(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, artist, title string) {
		tr := match.Parse(artist, title)
		if match.Normalize(tr.Title) != tr.Title {
			t.Errorf("Parse returned an unnormalized title %q", tr.Title)
		}
		for _, a := range tr.Artists {
			if match.Normalize(a) != a {
				t.Errorf("Parse returned an unnormalized artist %q", a)
			}
		}
	})
}

func FuzzScore(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s[0], s[1], 300, s[0], s[1], 300)
		f.Add(s[0], s[1], 300, "someone else", "another song", 0)
	}
	f.Fuzz(func(t *testing.T, a1, t1 string, d1 int, a2, t2 string, d2 int) {
		want, got := match.Parse(a1, t1), match.Parse(a2, t2)
		sc := match.Score(want, d1, got, d2)
		if sc.Score < 0 || sc.Score > 1 {
			t.Errorf("Score = %v, want within [0, 1]; why: %s", sc.Score, sc.Why)
		}
		// A track against itself, same duration, is a perfect match —
		// with two carve-outs that mirror decisions already made inside
		// Score, not new ones invented for this test:
		//
		//   - a title that normalizes to "" is excluded because ratio()
		//     deliberately scores two empty titles 0, not 1 — see its
		//     doc comment: "not evidence of a match".
		//   - an artist field that normalizes to no tokens at all is
		//     excluded for the identical reason: artistSimilarity
		//     returns 0, not 1, when either side has no parsed artists,
		//     because an empty set is no evidence of identity either.
		//     (splitArtists falls back to the whole normalized string
		//     when a name collides with a separator token, e.g. an
		//     artist literally called "X" — see its doc comment — but a
		//     genuinely punctuation-only field still parses to none.)
		//
		// The equality also carries a small epsilon rather than being
		// exact: weightTitle+weightArtist+weightVersion+weightDuration
		// is a compile-time constant expression, which the Go compiler
		// folds with arbitrary-precision arithmetic to exactly 1.0, but
		// Score's num is the same weights summed at *runtime* after
		// multiplying by variables — rounded at each step — so num/den
		// lands one ULP under 1.0 even for a bit-for-bit identical
		// track. That is ordinary floating-point non-associativity, not
		// a scoring defect: nine orders of magnitude below anything that
		// could move a verdict at the 0.85/0.60 thresholds.
		if a1 == a2 && t1 == t2 && d1 == d2 &&
			want.Title != "" && len(want.Artists) != 0 &&
			math.Abs(sc.Score-1) > 1e-9 {
			t.Errorf("identical tracks scored %v, want ~1; why: %s", sc.Score, sc.Why)
		}
	})
}
