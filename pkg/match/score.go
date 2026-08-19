package match

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// Candidate is a Spotify search result being considered for a DI.fm like.
type Candidate struct {
	ID          string
	Artist      string
	Title       string
	DurationSec int
	ISRC        string
}

// Scored pairs a candidate with its confidence and a human-readable
// rationale. The rationale is persisted into the review queue so a human
// reviewing a borderline call can see why the scorer hesitated.
type Scored struct {
	Candidate
	Score float64
	Why   string
}

// Component weights. Title and artist carry the identity; version and
// duration exist to separate the *edits* of a correctly identified track,
// which is where naive matching goes wrong on electronic music.
const (
	weightTitle    = 0.40
	weightArtist   = 0.30
	weightVersion  = 0.20
	weightDuration = 0.10

	// Applied when the two versions are mutually exclusive recordings
	// (a radio edit is not an extended mix). A penalty rather than a
	// veto: metadata is noisy enough that a hard reject would lose real
	// matches, but the result must land well below the auto-add bar.
	hardConflictPenalty = 0.75
)

// ScoreCandidate rates a Candidate against the wanted track and returns a
// fully populated Scored.
//
// Prefer this over Score for anything holding a Candidate. Score leaves
// the embedded Candidate zero for the caller to fill in, which is easy
// to forget and yields a Scored that looks complete and is not.
func ScoreCandidate(want Track, wantDur int, cand Candidate) Scored {
	s := Score(want, wantDur, Parse(cand.Artist, cand.Title), cand.DurationSec)
	s.Candidate = cand
	return s
}

// Score rates a candidate against the wanted track in [0,1].
//
// The returned Scored carries the score and rationale only; its embedded
// Candidate is left zero. Callers working from a Candidate should use
// ScoreCandidate instead.
//
// wantDur and gotDur are runtimes in seconds; zero means unknown, in
// which case the duration component is dropped and its weight is
// redistributed across the remaining components rather than counted as
// a mismatch.
func Score(want Track, wantDur int, got Track, gotDur int) Scored {
	titleSim := ratio(want.Title, got.Title)
	artistSim := artistSimilarity(want.Artists, got.Artists)
	version := versionScore(want.Version, got.Version)

	num := weightTitle*titleSim + weightArtist*artistSim + weightVersion*version.score
	den := weightTitle + weightArtist + weightVersion

	haveDurations := wantDur > 0 && gotDur > 0
	if haveDurations {
		num += weightDuration * durationScore(wantDur, gotDur)
		den += weightDuration
	}

	// Artist is not merely one weighted component: a wrong artist means a
	// wrong track no matter how exactly the title agrees, and same-title
	// collisions across artists are common. So artist similarity also
	// gates the total, steeply once it drops below the plausible band.
	score := (num / den) * artistGate(artistSim)

	var why []string
	if version.conflict {
		score *= hardConflictPenalty
		reason := version.reason
		if reason == "" {
			reason = "version conflict: " + want.Version.Kind.String() + " vs " + got.Version.Kind.String()
		}
		why = append(why, reason)
	}
	if haveDurations && absInt(wantDur-gotDur) > 30 {
		why = append(why, "runtime differs by "+strconv.Itoa(absInt(wantDur-gotDur))+"s")
	}
	if artistSim < 0.7 {
		why = append(why, "artist mismatch")
	}
	if titleSim < 0.7 {
		why = append(why, "title mismatch")
	}
	if len(why) == 0 {
		why = append(why, "clean match")
	}

	return Scored{Score: clamp01(score), Why: strings.Join(why, "; ")}
}

// versionVerdict rates version agreement. conflict marks a pair that
// cannot be the same recording; reason, when set, replaces the generic
// conflict rationale with something more specific.
type versionVerdict struct {
	score    float64
	conflict bool
	reason   string
}

// versionScore rates version agreement and reports whether the pair is a
// hard conflict — two versions that cannot be the same recording.
func versionScore(a, b Version) versionVerdict {
	if a.Kind == b.Kind {
		if a.Kind == VersionRemix {
			switch {
			case a.Remixer != "" && b.Remixer != "":
				// Two remixes match only if it's the same remixer.
				if r := ratio(a.Remixer, b.Remixer); r < 0.8 {
					return versionVerdict{score: r * 0.3, conflict: true}
				}
			case a.Remixer != "" || b.Remixer != "":
				// Exactly one side names a remixer. There is nothing to
				// compare it against, so this is not evidence of a match:
				// "Extended Remix" is every bit as consistent with some
				// other remixer's cut as with this one. Treating it as
				// agreement auto-added the wrong remix at "clean match",
				// and an add-only sync cannot take that back.
				//
				// Two *unnamed* remixes fall through to full credit
				// instead — neither side named anyone, so there is no
				// discrepancy, just a pair of loose descriptors.
				named := a.Remixer
				if named == "" {
					named = b.Remixer
				}
				return versionVerdict{
					score:    0.55,
					conflict: true,
					reason:   "unconfirmed remix: one side names no remixer, the other names " + named,
				}
			}
		}
		return versionVerdict{score: 1.0}
	}

	// Spotify frequently omits the descriptor that DI.fm carries, so an
	// unknown on one side is weak evidence, never a conflict.
	if a.Kind == VersionUnknown || b.Kind == VersionUnknown {
		other := a.Kind
		if other == VersionUnknown {
			other = b.Kind
		}
		if other == VersionOriginal {
			return versionVerdict{score: 0.85} // "Original Mix" vs bare title: the usual case
		}
		return versionVerdict{score: 0.55}
	}

	// A remix is a different recording from any non-remix.
	if a.Kind == VersionRemix || b.Kind == VersionRemix {
		return versionVerdict{score: 0.10, conflict: true}
	}

	pair := func(x, y VersionKind) bool {
		return (a.Kind == x && b.Kind == y) || (a.Kind == y && b.Kind == x)
	}
	switch {
	case pair(VersionRadio, VersionExtended):
		return versionVerdict{score: 0.0, conflict: true} // the canonical failure this scorer exists to prevent
	case pair(VersionOriginal, VersionExtended):
		return versionVerdict{score: 0.45, conflict: true}
	case pair(VersionOriginal, VersionRadio):
		return versionVerdict{score: 0.35, conflict: true}
	case pair(VersionLive, VersionOriginal), pair(VersionLive, VersionExtended),
		pair(VersionAcoustic, VersionOriginal), pair(VersionInstrumental, VersionOriginal):
		return versionVerdict{score: 0.15, conflict: true}
	}
	return versionVerdict{score: 0.3, conflict: true}
}

// artistGate scales the whole score down when the artist looks wrong.
// Above the plausible band it is a no-op, so genuine matches are untouched.
func artistGate(artistSim float64) float64 {
	const plausible = 0.7
	if artistSim >= plausible {
		return 1.0
	}
	return 0.45 + (artistSim/plausible)*0.55
}

// durationScore is 1.0 within 3s and decays linearly to 0 at 60s apart.
func durationScore(a, b int) float64 {
	d := absInt(a - b)
	switch {
	case d <= 3:
		return 1.0
	case d >= 60:
		return 0.0
	default:
		return 1.0 - float64(d-3)/57.0
	}
}

// artistSimilarity compares unordered artist sets: every wanted artist is
// matched to its best counterpart, so "A & B" vs "B and A" scores 1.0.
func artistSimilarity(want, got []string) float64 {
	if len(want) == 0 || len(got) == 0 {
		return 0
	}
	var total float64
	for _, w := range want {
		best := 0.0
		for _, g := range got {
			if r := ratio(w, g); r > best {
				best = r
			}
		}
		total += best
	}
	score := total / float64(len(want))

	// A credited collaborator missing on one side shouldn't read as a
	// wrong artist, but it is mild evidence against.
	if len(got) > len(want) {
		score *= 0.97
	}
	return score
}

// ratio is a normalized Levenshtein similarity in [0,1].
//
// The empty check precedes the equality check deliberately: two titles
// that both normalize to "" (a title of pure punctuation, say) are not
// evidence of a match, and returning 1 for that pair would clear the
// auto-add bar on no evidence at all.
//
// The denominator counts runes, not bytes. levenshtein works on runes,
// so dividing by len() would inflate the denominator two- to four-fold
// for any input diacriticFold does not reduce to ASCII — Cyrillic,
// Greek, CJK — and systematically overstate similarity for them.
func ratio(a, b string) float64 {
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 1
	}
	d := levenshtein(a, b)
	longest := max(utf8.RuneCountInString(a), utf8.RuneCountInString(b))
	return 1 - float64(d)/float64(longest)
}

// levenshtein is the standard two-row dynamic-programming edit distance.
func levenshtein(a, b string) int {
	ar, br := []rune(a), []rune(b)
	if len(ar) < len(br) {
		ar, br = br, ar
	}
	prev := make([]int, len(br)+1)
	curr := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		curr[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			curr[j] = min(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(br)]
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func clamp01(f float64) float64 {
	switch {
	case f < 0:
		return 0
	case f > 1:
		return 1
	default:
		return f
	}
}
