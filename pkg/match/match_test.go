package match_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mjrossi/difm-spotify-sync/pkg/match"
)

// Thresholds mirror the runtime defaults. Tests assert against these so a
// weight change that silently degrades matching fails here first.
const (
	autoThreshold   = 0.85
	reviewThreshold = 0.60
)

func TestParse(t *testing.T) {
	tests := []struct {
		name          string
		artist, title string
		want          match.Track
	}{
		{
			name:   "parenthesised remix names the remixer",
			artist: "Funk D'Void & Berny",
			title:  "Junkies (Joe Silva Remix)",
			want: match.Track{
				Artists: []string{"funk dvoid", "berny"},
				Title:   "junkies",
				Version: match.Version{Kind: match.VersionRemix, Remixer: "joe silva", Raw: "Joe Silva Remix"},
			},
		},
		{
			name:   "dash suffix is a version, not part of the title",
			artist: "Deadmau5",
			title:  "Strobe - Radio Edit",
			want: match.Track{
				Artists: []string{"deadmau5"},
				Title:   "strobe",
				Version: match.Version{Kind: match.VersionRadio, Raw: "Radio Edit"},
			},
		},
		{
			name:   "feat in the title moves to Featuring",
			artist: "Above & Beyond",
			title:  "Sun & Moon feat. Richard Bedford",
			want: match.Track{
				Artists:   []string{"above", "beyond"},
				Title:     "sun moon",
				Featuring: []string{"richard bedford"},
			},
		},
		{
			name:   "diacritics fold",
			artist: "Björk",
			title:  "Jóga",
			want: match.Track{
				Artists: []string{"bjork"},
				Title:   "joga",
			},
		},
		{
			name:   "a compound descriptor is a remix, not its modifier",
			artist: "Deadmau5",
			title:  "Strobe (Extended Remix)",
			want: match.Track{
				Artists: []string{"deadmau5"},
				Title:   "strobe",
				// "extended" is how it was cut, not who cut it.
				Version: match.Version{Kind: match.VersionRemix, Raw: "Extended Remix"},
			},
		},
		{
			name:   "a remixer whose name contains a modifier word is kept",
			artist: "Quench",
			title:  "Dreams (Radio Slave Remix)",
			want: match.Track{
				Artists: []string{"quench"},
				Title:   "dreams",
				Version: match.Version{Kind: match.VersionRemix, Remixer: "radio slave", Raw: "Radio Slave Remix"},
			},
		},
		{
			name:   "a trailing modifier is stripped from a real remixer name",
			artist: "Funk D'Void & Berny",
			title:  "Junkies (Joe Silva Extended Remix)",
			want: match.Track{
				Artists: []string{"funk dvoid", "berny"},
				Title:   "junkies",
				// Same remixer as a bare "Joe Silva Remix" — "extended"
				// describes the cut, so it must not fork the identity.
				Version: match.Version{Kind: match.VersionRemix, Remixer: "joe silva", Raw: "Joe Silva Extended Remix"},
			},
		},
		{
			name:   "the operative marker is the last one, not the first",
			artist: "Quench",
			title:  "Dreams (Bootleg Remix)",
			want: match.Track{
				Artists: []string{"quench"},
				Title:   "dreams",
				// Splitting on the first marker left "bootleg" as a remixer.
				Version: match.Version{Kind: match.VersionRemix, Raw: "Bootleg Remix"},
			},
		},
		{
			name:   "a marker buried inside a word is not a descriptor",
			artist: "Ableton Sound",
			title:  "Vipassana Meditation",
			want: match.Track{
				Artists: []string{"ableton sound"},
				Title:   "vipassana meditation",
				// "vip" as a substring used to make this a remix.
				Version: match.Version{Kind: match.VersionUnknown},
			},
		},
		{
			name:   "original mix is recognized as the primary cut",
			artist: "Gabriel & Dresden",
			title:  "Tracking Treasure Down (Original Mix)",
			want: match.Track{
				Artists: []string{"gabriel", "dresden"},
				Title:   "tracking treasure down",
				Version: match.Version{Kind: match.VersionOriginal, Raw: "Original Mix"},
			},
		},
		{
			// Pins: a whole-name artist that IS a separator word must
			// still parse as an artist. An unanchored \bx\b matches the
			// entire field, and Split then returns only the two empty
			// pieces around it, losing the name outright.
			name:   "an artist whose whole name is a separator word survives",
			artist: "X",
			title:  "Rain",
			want: match.Track{
				Artists: []string{"x"},
				Title:   "rain",
			},
		},
		{
			// Same failure, word-alternative form.
			name:   "an artist whose whole name is the word AND survives",
			artist: "AND",
			title:  "Rain",
			want: match.Track{
				Artists: []string{"and"},
				Title:   "rain",
			},
		},
		{
			// Regression guard the other direction: a genuinely
			// punctuation-only field must still parse to no artists —
			// there was never a name to recover here.
			name:   "a punctuation-only artist field stays empty",
			artist: "!!!",
			title:  "Rain",
			want: match.Track{
				Title: "rain",
			},
		},
		{
			// A real "x" separator between two names must still split
			// them — this is the behavior the token exists for.
			name:   "a genuine x separator between two names still splits",
			artist: "Chris Lake x Fisher",
			title:  "Rain",
			want: match.Track{
				Artists: []string{"chris lake", "fisher"},
				Title:   "rain",
			},
		},
		{
			// Pins: the leading word of a real name must not be eaten
			// just because it matches a separator token. An unanchored
			// \bx\b turned "X Ambassadors" into "ambassadors".
			name:   "a separator word that leads a real name is kept whole",
			artist: "X Ambassadors",
			title:  "Rain",
			want: match.Track{
				Artists: []string{"x ambassadors"},
				Title:   "rain",
			},
		},
		{
			// Pins the worst-shaped version of the same bug: a
			// collaboration whose first member's name collides with the
			// separator token must not collapse to the other member
			// alone — that is a false-positive match on an add-only
			// sync, not just a lost artist.
			name:   "a collab where one member's name is the separator token still splits",
			artist: "X & Beta",
			title:  "Rain",
			want: match.Track{
				Artists: []string{"x", "beta"},
				Title:   "rain",
			},
		},
		{
			// Pins the Oxford comma. Requiring whitespace before a word
			// separator broke it: the comma branch eats the space, so
			// "and" had none in front and "and c" parsed as an artist.
			name:   "an Oxford comma before and still splits cleanly",
			artist: "Alpha, Beta, and Gamma",
			title:  "Rain",
			want: match.Track{
				Artists: []string{"alpha", "beta", "gamma"},
				Title:   "rain",
			},
		},
		{
			// The comma absorbs "and", never "x": a name that starts with
			// a separator word must survive coming after a comma too.
			name:   "a separator word leading a name after a comma is kept whole",
			artist: "Alpha, X Ambassadors",
			title:  "Rain",
			want: match.Track{
				Artists: []string{"alpha", "x ambassadors"},
				Title:   "rain",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := match.Parse(tc.artist, tc.title)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("Parse() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestScore_EditsDoNotCollide is the test this scorer exists to pass: two
// different edits of the same track must never auto-add as each other.
func TestScore_EditsDoNotCollide(t *testing.T) {
	tests := []struct {
		name                  string
		wantArtist, wantTitle string
		wantDur               int
		gotArtist, gotTitle   string
		gotDur                int
		maxScore              float64
		reason                string
	}{
		{
			name:       "radio edit must not match extended mix",
			wantArtist: "Deadmau5", wantTitle: "Strobe (Extended Mix)", wantDur: 634,
			gotArtist: "Deadmau5", gotTitle: "Strobe - Radio Edit", gotDur: 190,
			maxScore: reviewThreshold,
			reason:   "same track, incompatible edits, 7min runtime gap",
		},
		{
			name:       "different remixer must not match",
			wantArtist: "Funk D'Void & Berny", wantTitle: "Junkies (Joe Silva Remix)", wantDur: 442,
			gotArtist: "Funk D'Void & Berny", gotTitle: "Junkies (Marco Bailey Remix)", gotDur: 440,
			maxScore: autoThreshold,
			reason:   "a different remix is a different recording",
		},
		{
			name:       "remix must not match the original",
			wantArtist: "DJ Rax", wantTitle: "Air Race (Spiritchaser Remix)", wantDur: 480,
			gotArtist: "DJ Rax", gotTitle: "Air Race (Original Mix)", gotDur: 475,
			maxScore: autoThreshold,
			reason:   "remix vs original are distinct recordings",
		},
		{
			// Regression: a remixer prefix built entirely from modifiers
			// yields Remixer == "", and versionScore only compared
			// remixers when *both* were set — so an unnamed remix took
			// the same-kind fall-through to full credit and auto-added
			// at "clean match". This is the paired direction of
			// TestScore_RealMatchesSucceed's "same remix described with
			// and without a modifier": both sides must hold at once.
			name:       "an unnamed remix must not auto-add a named one",
			wantArtist: "Deadmau5", wantTitle: "Strobe (Extended Remix)", wantDur: 420,
			gotArtist: "Deadmau5", gotTitle: "Strobe (Eric Prydz Remix)", gotDur: 405,
			maxScore: autoThreshold,
			reason:   "an unnamed remix is no evidence it is *this* remixer's cut",
		},
		{
			name:       "a modifier-only descriptor must not auto-add a named remix",
			wantArtist: "Fleetwood Mac", wantTitle: "Dreams (Club Remix)", wantDur: 420,
			gotArtist: "Fleetwood Mac", gotTitle: "Dreams (Radio Slave Remix)", gotDur: 405,
			maxScore: autoThreshold,
			reason:   "\"Club\" names nobody; Radio Slave is a specific remixer",
		},
		{
			// Regression: ratio() divided a rune-based edit distance by a
			// byte length, inflating the denominator ~2x for non-Latin
			// scripts and overstating similarity for unrelated names.
			name:       "unrelated non-Latin artists must not match",
			wantArtist: "Пётр Наливайко", wantTitle: "Ночь", wantDur: 400,
			gotArtist: "Кирилл Демидов", gotTitle: "Ночь", gotDur: 402,
			maxScore: reviewThreshold,
			reason:   "different Cyrillic artists sharing a common title",
		},
		{
			name:       "same title by a different artist must not match",
			wantArtist: "Elements Of Life", wantTitle: "Live Your Life For Today", wantDur: 555,
			gotArtist: "Some Other Act", gotTitle: "Live Your Life For Today", gotDur: 550,
			maxScore: reviewThreshold,
			reason:   "title collisions across artists are common",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			want := match.Parse(tc.wantArtist, tc.wantTitle)
			got := match.Parse(tc.gotArtist, tc.gotTitle)
			s := match.Score(want, tc.wantDur, got, tc.gotDur)
			if s.Score >= tc.maxScore {
				t.Errorf("Score = %.3f, want < %.2f (%s)\n  why: %s",
					s.Score, tc.maxScore, tc.reason, s.Why)
			}
		})
	}
}

// TestScore_RealMatchesSucceed guards the opposite failure: a scorer that
// rejects everything would pass the test above trivially.
func TestScore_RealMatchesSucceed(t *testing.T) {
	tests := []struct {
		name                  string
		wantArtist, wantTitle string
		wantDur               int
		gotArtist, gotTitle   string
		gotDur                int
	}{
		{
			name:       "exact match",
			wantArtist: "Funk D'Void & Berny", wantTitle: "Junkies (Joe Silva Remix)", wantDur: 442,
			gotArtist: "Funk D'Void & Berny", gotTitle: "Junkies (Joe Silva Remix)", gotDur: 442,
		},
		{
			name:       "Spotify omits the Original Mix descriptor",
			wantArtist: "Gabriel & Dresden", wantTitle: "Tracking Treasure Down (Original Mix)", wantDur: 501,
			gotArtist: "Gabriel & Dresden", gotTitle: "Tracking Treasure Down", gotDur: 498,
		},
		{
			name:       "artist order differs and separator differs",
			wantArtist: "Elements Of Life, Josh Milan", wantTitle: "Live Your Life For Today (Roots NYC Main Mix)", wantDur: 555,
			gotArtist: "Josh Milan & Elements Of Life", gotTitle: "Live Your Life For Today (Roots NYC Main Mix)", gotDur: 557,
		},
		{
			name:       "featured artist credited in different field",
			wantArtist: "Above & Beyond", wantTitle: "Sun & Moon feat. Richard Bedford", wantDur: 400,
			gotArtist: "Above & Beyond feat. Richard Bedford", gotTitle: "Sun & Moon", gotDur: 402,
		},
		{
			name:       "the same remix described with and without a modifier",
			wantArtist: "Deadmau5", wantTitle: "Strobe (Extended Remix)", wantDur: 634,
			gotArtist: "Deadmau5", gotTitle: "Strobe (Remix)", gotDur: 632,
		},
		{
			// Regression: remixerName was all-or-nothing, so a trailing
			// modifier made "joe silva extended" a *different* remixer
			// from "joe silva" and the pair scored as a hard conflict.
			name:       "the same remixer with a trailing modifier",
			wantArtist: "Funk D'Void & Berny", wantTitle: "Junkies (Joe Silva Extended Remix)", wantDur: 442,
			gotArtist: "Funk D'Void & Berny", gotTitle: "Junkies (Joe Silva Remix)", gotDur: 440,
		},
		{
			name:       "diacritics and punctuation differ",
			wantArtist: "Björk", wantTitle: "Jóga", wantDur: 300,
			gotArtist: "Bjork", gotTitle: "Joga", gotDur: 301,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			want := match.Parse(tc.wantArtist, tc.wantTitle)
			got := match.Parse(tc.gotArtist, tc.gotTitle)
			s := match.Score(want, tc.wantDur, got, tc.gotDur)
			if s.Score < autoThreshold {
				t.Errorf("Score = %.3f, want >= %.2f\n  why: %s", s.Score, autoThreshold, s.Why)
			}
		})
	}
}

// TestScore_UnknownDurationIsNotAMismatch: DI.fm always reports length,
// but a zero must not be read as "0 seconds" and tank an otherwise clean match.
func TestScore_UnknownDurationIsNotAMismatch(t *testing.T) {
	want := match.Parse("Funk D'Void & Berny", "Junkies (Joe Silva Remix)")
	got := match.Parse("Funk D'Void & Berny", "Junkies (Joe Silva Remix)")

	withDur := match.Score(want, 442, got, 442)
	noDur := match.Score(want, 0, got, 0)

	if noDur.Score < autoThreshold {
		t.Errorf("unknown-duration score = %.3f, want >= %.2f", noDur.Score, autoThreshold)
	}
	if diff := withDur.Score - noDur.Score; diff > 0.05 || diff < -0.05 {
		t.Errorf("dropping duration shifted score by %.3f, want ~0", diff)
	}
}

// TestScore_AnArtistNamedLikeASeparatorIsStillAnArtist guards the worst
// shape of the splitArtists bug: an unanchored separator token doesn't
// just lose an artist name, it can make a *different* artist's track
// auto-add at full confidence in a sync that only ever adds tracks and
// never removes them.
func TestScore_AnArtistNamedLikeASeparatorIsStillAnArtist(t *testing.T) {
	t.Run("identical X/Rain pair clears the auto bar", func(t *testing.T) {
		want := match.Parse("X", "Rain")
		got := match.Parse("X", "Rain")
		s := match.Score(want, 300, got, 300)
		if s.Score < autoThreshold {
			t.Errorf("Score = %.4f, want >= %.2f (was 0.3150 with the empty-artist bug)\n  why: %s",
				s.Score, autoThreshold, s.Why)
		}
	})

	t.Run("X does not match a different artist", func(t *testing.T) {
		want := match.Parse("X", "Rain")
		got := match.Parse("Deadmau5", "Rain")
		s := match.Score(want, 300, got, 300)
		if s.Score >= reviewThreshold {
			t.Errorf("Score = %.4f, want < %.2f\n  why: %s", s.Score, reviewThreshold, s.Why)
		}
	})

	t.Run("punctuation-only artist pair is not evidence of a match", func(t *testing.T) {
		want := match.Parse("!!!", "Rain")
		got := match.Parse("!!!", "Rain")
		s := match.Score(want, 300, got, 300)
		if s.Score >= reviewThreshold {
			t.Errorf("Score = %.4f, want < %.2f\n  why: %s", s.Score, reviewThreshold, s.Why)
		}
	})

	t.Run("a collaboration must not auto-add as one member's solo track", func(t *testing.T) {
		want := match.Parse("X & Beta", "Rain")
		got := match.Parse("Beta", "Rain")
		s := match.Score(want, 300, got, 300)
		if s.Score >= autoThreshold {
			t.Errorf("Score = %.4f, want < %.2f — a collaboration auto-added as its solo member\n  why: %s",
				s.Score, autoThreshold, s.Why)
		}
	})
}

func TestNormalize(t *testing.T) {
	tests := []struct{ in, want string }{
		{"  Strobe  ", "strobe"},
		{"Sun & Moon", "sun moon"},
		{"D.A.V.E. The Drummer", "d a v e the drummer"},
		{"Funk D'Void", "funk dvoid"},
		{"Björk", "bjork"},
		{"Don’t Stop", "dont stop"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := match.Normalize(tc.in); got != tc.want {
			t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestScore_UnnormalizableTitlesDoNotMatch: two titles made entirely of
// punctuation both normalize to "", and an equality-first similarity would
// score that pair 1.0 and auto-add a wrong track on no evidence at all.
func TestScore_UnnormalizableTitlesDoNotMatch(t *testing.T) {
	want := match.Parse("Various Artists", "!!!")
	got := match.Parse("Various Artists", "???")

	s := match.Score(want, 300, got, 300)
	if s.Score >= autoThreshold {
		t.Errorf("Score = %.3f for two empty-normalizing titles, want < %.2f\n  why: %s",
			s.Score, autoThreshold, s.Why)
	}
}
