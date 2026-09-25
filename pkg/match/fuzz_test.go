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
	{"long", strings.Repeat("a very long title ", 50)},
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

// FuzzParse is mostly a panic detector rather than a property test: Parse
// calls Normalize on every field it sets, so "the output is normalized"
// is a tautology it cannot fail short of a panic. It still earns its own
// target because Parse's regex pipeline (dashSuffixRe, parenGroupRe,
// featRe, artistSplit chained together) is the part of this package most
// likely to panic on adversarial input, and because the assertions catch
// a *build* that forgets to normalize a new field.
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
		// Featuring goes through splitArtists too — the same code path
		// this bug lived in — so it gets the same check.
		for _, a := range tr.Featuring {
			if match.Normalize(a) != a {
				t.Errorf("Parse returned an unnormalized featured artist %q", a)
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
		// !(>= 0 && <= 1) rather than (< 0 || > 1): a NaN score compares
		// false against every ordering operator, so the inverted form is
		// what actually catches one instead of silently passing it through.
		if !(sc.Score >= 0 && sc.Score <= 1) {
			t.Errorf("Score = %v, want within [0, 1]; why: %s", sc.Score, sc.Why)
		}
		// A track against itself, same duration, is a perfect match —
		// with two carve-outs that mirror decisions already made inside
		// Score, not new ones invented for this test:
		//
		//   - a title that normalizes to "" is excluded because ratio()
		//     deliberately scores two empty titles 0, not 1 — see its
		//     doc comment: "not evidence of a match".
		//   - an artist field that normalizes to no tokens at all (e.g.
		//     "!!!") is excluded for the identical reason:
		//     artistSimilarity returns 0, not 1, when either side has no
		//     parsed artists. This is checked on the *raw* field with
		//     Normalize, not by inspecting want.Artists — with
		//     artistSplit now anchored to real separators, an artist
		//     that merely collides with a separator word (e.g. "X")
		//     parses to a real, non-empty artist list, and hiding that
		//     case behind a len(want.Artists) check would blind this
		//     fuzz target to the exact bug FuzzScore's seed corpus found.
		//
		// The equality also carries a small epsilon rather than being
		// exact, because floating-point addition is not associative and
		// the two sides of the equal-weight case take different paths to
		// 1.0. Without a duration, den is a compile-time constant
		// expression the compiler folds with arbitrary-precision
		// arithmetic; num is the same weights multiplied by runtime
		// variables and summed at runtime, rounding at each step, so it
		// lands one ULP under den. With a duration, den itself becomes a
		// runtime addition too — yet num still lands one ULP under it:
		// same one-ULP gap, but now because num's four-term runtime sum
		// rounds differently than den's three-compile-time-terms-plus-
		// one-runtime-add, not because one side is compile-time and the
		// other isn't. Measured gap in both cases: 1.11e-16 (one ULP),
		// nine orders of magnitude below anything that could move a
		// verdict at the 0.85/0.60 thresholds.
		if a1 == a2 && t1 == t2 && d1 == d2 &&
			want.Title != "" && match.Normalize(a1) != "" &&
			math.Abs(sc.Score-1) > 1e-15 {
			t.Errorf("identical tracks scored %v, want ~1; why: %s", sc.Score, sc.Why)
		}
	})
}
