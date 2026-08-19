// Package match turns messy radio metadata into comparable track
// identities and scores candidate matches.
//
// Matching electronic music is the hard part of this project. DI.fm
// reports free-form "Artist - Title" strings from radio metadata while
// Spotify has its own conventions, and the same work appears as
// "Strobe", "Strobe (Original Mix)", "Strobe - Radio Edit" and "Strobe
// (Extended Mix)". Those are genuinely different recordings with
// different runtimes, so version information is parsed out and compared
// rather than stripped — collapsing them yields a playlist full of the
// wrong edits.
package match

import (
	"regexp"
	"strings"
	"unicode"
)

// VersionKind classifies the mix/edit variant of a recording.
type VersionKind int

const (
	VersionUnknown VersionKind = iota
	VersionOriginal
	VersionExtended
	VersionRadio
	VersionRemix
	VersionInstrumental
	VersionLive
	VersionAcoustic
)

func (v VersionKind) String() string {
	switch v {
	case VersionOriginal:
		return "original"
	case VersionExtended:
		return "extended"
	case VersionRadio:
		return "radio"
	case VersionRemix:
		return "remix"
	case VersionInstrumental:
		return "instrumental"
	case VersionLive:
		return "live"
	case VersionAcoustic:
		return "acoustic"
	default:
		return "unknown"
	}
}

// Version is the parsed mix/edit descriptor of a title.
type Version struct {
	Kind    VersionKind
	Remixer string // normalized; meaningful only when Kind == VersionRemix
	Raw     string
}

// Track is a normalized, comparable track identity.
type Track struct {
	Artists   []string
	Title     string
	Featuring []string
	Version   Version
}

var (
	parenGroupRe = regexp.MustCompile(`\s*[\(\[]([^)\]]*)[\)\]]\s*`)
	dashSuffixRe = regexp.MustCompile(`(?i)\s+-\s+([^-]*?(?:mix|edit|remix|version|dub|vip|bootleg|instrumental|live|acoustic))\s*$`)
	featRe       = regexp.MustCompile(`(?i)\s*\b(?:feat\.?|ft\.?|featuring)\s+(.+)$`)
	artistSplit  = regexp.MustCompile(`(?i)\s*(?:,|&|\+|\bx\b|\bvs\.?\b|\band\b|\bwith\b)\s*`)
	nonAlnumRe   = regexp.MustCompile(`[^\p{L}\p{N}\s]+`)
	spaceRe      = regexp.MustCompile(`\s+`)
	// Apostrophes are elisions, not word boundaries — "D'Void" is one
	// token and "Don't" is one word. Stripped before the general
	// punctuation rule, which would otherwise split them.
	apostropheRe = regexp.MustCompile(`['\x60]`)
)

// diacriticFold covers the Latin-script accents that actually show up in
// electronic-music metadata. Hand-rolled rather than pulling in
// golang.org/x/text, per the project's stdlib-first rule.
var diacriticFold = map[rune]string{
	'á': "a", 'à': "a", 'â': "a", 'ä': "a", 'ã': "a", 'å': "a", 'ā': "a",
	'é': "e", 'è': "e", 'ê': "e", 'ë': "e", 'ē': "e",
	'í': "i", 'ì': "i", 'î': "i", 'ï': "i", 'ī': "i",
	'ó': "o", 'ò': "o", 'ô': "o", 'ö': "o", 'õ': "o", 'ø': "o", 'ō': "o",
	'ú': "u", 'ù': "u", 'û': "u", 'ü': "u", 'ū': "u",
	'ç': "c", 'ñ': "n", 'ß': "ss", 'æ': "ae", 'œ': "oe", 'ð': "d", 'þ': "th",
	'ł': "l", 'ý': "y", 'ÿ': "y", 'š': "s", 'ž': "z", 'č': "c", 'ř': "r",
	'’': "'", '‘': "'", '“': `"`, '”': `"`,
	'–': "-", '—': "-",
}

func stripDiacritics(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if folded, ok := diacriticFold[r]; ok {
			b.WriteString(folded)
			continue
		}
		if unicode.Is(unicode.Mn, r) {
			continue // combining mark left by NFD-ish input
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Normalize lowercases, folds diacritics, drops punctuation and collapses
// whitespace.
func Normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = stripDiacritics(s)
	s = apostropheRe.ReplaceAllString(s, "")
	s = nonAlnumRe.ReplaceAllString(s, " ")
	s = spaceRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// remixMarkers are the words that make a descriptor a remix. They are
// tested before every other kind, because a remix descriptor routinely
// carries another kind's keyword as a modifier — "Extended Remix",
// "Radio Slave Remix", "Instrumental Remix" — and classifying those by
// the modifier loses both the kind and the remixer name.
var remixMarkers = []string{"remix", "rmx", "bootleg", "vip"}

// isRemixMarker reports whether a single normalized token is a remix
// marker. Matching is per-token rather than by substring: "vip" as a
// substring also fires on "vipassana", and a marker buried mid-word is
// not a descriptor. A short suffix is allowed so "Remixes"/"Remixed"
// still classify, which a bare equality test would drop.
func isRemixMarker(tok string) bool {
	for _, marker := range remixMarkers {
		if strings.HasPrefix(tok, marker) && len(tok)-len(marker) <= 2 {
			return true
		}
	}
	return false
}

// versionModifiers are words that describe *how* a remix was cut rather
// than who cut it. They are stripped from the tail of a remixer prefix:
// "Extended Remix" has no remixer, "Joe Silva Extended Remix" is remixed
// by Joe Silva, and "Radio Slave Remix" is remixed by Radio Slave.
//
// Stripping is tail-anchored precisely because of that last case. A
// modifier word is only a modifier when it sits against the marker;
// "Radio Slave" is an artist whose name opens with one, and dropping
// every modifier token wherever it appeared would rename them "Slave".
var versionModifiers = map[string]bool{
	"extended": true, "radio": true, "edit": true, "instrumental": true,
	"live": true, "acoustic": true, "club": true, "original": true,
	"vocal": true, "dub": true, "mix": true, "version": true, "short": true,
	"long": true, "full": true, "re": true, "the": true,
}

// classifyVersion maps a raw descriptor onto a kind plus remixer.
func classifyVersion(raw string) Version {
	n := Normalize(raw)
	if n == "" {
		return Version{Kind: VersionUnknown}
	}
	v := Version{Raw: strings.TrimSpace(raw)}

	// Remix is tested first; see remixMarkers. The scan runs right to
	// left so the *last* marker token bounds the remixer name: in
	// "Bootleg Remix" the operative marker is the trailing one, and
	// splitting on the first would leave "bootleg" as a remixer.
	toks := strings.Fields(n)
	for i := len(toks) - 1; i >= 0; i-- {
		if !isRemixMarker(toks[i]) {
			continue
		}
		v.Kind = VersionRemix
		v.Remixer = remixerName(toks[:i])
		return v
	}

	switch {
	case strings.Contains(n, "original"):
		v.Kind = VersionOriginal
	case strings.Contains(n, "extended") || strings.Contains(n, "club mix") || strings.Contains(n, "12 inch"):
		v.Kind = VersionExtended
	case strings.Contains(n, "radio") || strings.Contains(n, "short edit"):
		v.Kind = VersionRadio
	case strings.Contains(n, "instrumental"):
		v.Kind = VersionInstrumental
	case strings.Contains(n, "acoustic"):
		v.Kind = VersionAcoustic
	case strings.Contains(n, "live"):
		v.Kind = VersionLive
	case strings.Contains(n, "mix"), strings.Contains(n, "edit"), strings.Contains(n, "version"):
		// A bare "Mix"/"Edit" denotes the primary cut, same as Original.
		v.Kind = VersionOriginal
	default:
		v.Kind = VersionUnknown
	}
	return v
}

// remixerName returns the remixer named by a remix descriptor's prefix
// tokens, or "" when the prefix is all modifiers and names nobody.
//
// Trailing modifiers are dropped so that "Joe Silva Extended Remix" and
// "Joe Silva Remix" agree on the remixer instead of reading as two
// different people — they are the same remixer, described at different
// lengths. See versionModifiers for why the strip is tail-anchored.
func remixerName(prefix []string) string {
	end := len(prefix)
	for end > 0 && (versionModifiers[prefix[end-1]] || isRemixMarker(prefix[end-1])) {
		end--
	}
	return strings.Join(prefix[:end], " ")
}

// Parse turns a raw artist and title into a comparable Track.
func Parse(artist, title string) Track {
	var t Track

	rawArtist := artist
	if m := featRe.FindStringSubmatch(rawArtist); m != nil {
		t.Featuring = append(t.Featuring, splitArtists(m[1])...)
		rawArtist = featRe.ReplaceAllString(rawArtist, "")
	}
	t.Artists = splitArtists(rawArtist)

	rawTitle := title
	if m := featRe.FindStringSubmatch(rawTitle); m != nil {
		t.Featuring = append(t.Featuring, splitArtists(m[1])...)
		rawTitle = featRe.ReplaceAllString(rawTitle, "")
	}

	var descriptors []string
	if m := dashSuffixRe.FindStringSubmatch(rawTitle); m != nil {
		descriptors = append(descriptors, m[1])
		rawTitle = dashSuffixRe.ReplaceAllString(rawTitle, "")
	}
	for _, m := range parenGroupRe.FindAllStringSubmatch(rawTitle, -1) {
		inner := m[1]
		if fm := featRe.FindStringSubmatch(" " + inner); fm != nil {
			t.Featuring = append(t.Featuring, splitArtists(fm[1])...)
			continue
		}
		descriptors = append(descriptors, inner)
	}
	rawTitle = parenGroupRe.ReplaceAllString(rawTitle, " ")
	t.Title = Normalize(rawTitle)

	// Prefer the most specific descriptor: a named remix outranks a bare mix.
	for _, d := range descriptors {
		v := classifyVersion(d)
		if v.Kind == VersionUnknown {
			continue
		}
		if t.Version.Kind == VersionUnknown || v.Kind == VersionRemix {
			t.Version = v
		}
	}
	return t
}

func splitArtists(s string) []string {
	var out []string
	for _, part := range artistSplit.Split(s, -1) {
		if n := Normalize(part); n != "" {
			out = append(out, n)
		}
	}
	return out
}
