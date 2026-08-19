package difm_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mjrossi/difm-spotify-sync/pkg/difm"
)

// newFixtureServer serves the captured payload on the first page and an
// empty array thereafter, mirroring how the real API terminates (short
// page; no total count, no pagination headers).
func newFixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	body, err := os.ReadFile("testdata/track_votes.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-API-Key"); got == "" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("Member Authentication required"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "1" {
			_, _ = w.Write(body)
			return
		}
		_, _ = w.Write([]byte("[]"))
	}))
}

// newPageServer serves body as page 1 and an empty page thereafter, for
// tests whose payload is deliberately inline rather than in testdata/.
func newPageServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "1" {
			_, _ = w.Write([]byte(body))
			return
		}
		_, _ = w.Write([]byte("[]"))
	}))
}

func newClient(t *testing.T, srv *httptest.Server) *difm.Client {
	t.Helper()
	c := difm.New("test-key", "10000001")
	c.BaseURL = srv.URL
	return c
}

func TestListLikedTracks(t *testing.T) {
	srv := newFixtureServer(t)
	defer srv.Close()

	got, err := newClient(t, srv).ListLikedTracks(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("ListLikedTracks: %v", err)
	}

	// The downvoted row is filtered out; the other three survive so that
	// non-track content is *recorded* rather than silently lost.
	if len(got) != 3 {
		t.Fatalf("got %d tracks, want 3 (downvote filtered out)", len(got))
	}

	// Oldest first, so a partial failure advances the watermark safely.
	for i := 1; i < len(got); i++ {
		if got[i].LikedAt.Before(got[i-1].LikedAt) {
			t.Errorf("results not oldest-first at %d: %s before %s",
				i, got[i].LikedAt, got[i-1].LikedAt)
		}
	}

	byID := map[int64]difm.Track{}
	for _, tr := range got {
		byID[tr.TrackID] = tr
	}

	track, ok := byID[3041427]
	if !ok {
		t.Fatal("real track 3041427 missing")
	}
	if track.Artist != "Funk D'Void & Berny" {
		t.Errorf("Artist = %q", track.Artist)
	}
	if track.Title != "Junkies (Joe Silva Remix)" {
		t.Errorf("Title = %q", track.Title)
	}
	if track.DurationSec != 442 {
		t.Errorf("DurationSec = %d, want 442", track.DurationSec)
	}
	if track.VoteID != 64755876 {
		t.Errorf("VoteID = %d", track.VoteID)
	}
	if track.Skip {
		t.Errorf("real track marked Skip (%s)", track.SkipReason)
	}
	if want := time.Date(2026, 2, 28, 16, 2, 17, 0, time.UTC); !track.LikedAt.Equal(want) {
		t.Errorf("LikedAt = %s, want %s (offset must be normalized to UTC)", track.LikedAt, want)
	}

	// A DJ mix and a mix-show episode are both flagged, not dropped:
	// searching Spotify for an hour-long set matches the wrong thing.
	if mix := byID[3041000]; !mix.Skip {
		t.Error("DJ mix (mix=true) should be flagged Skip")
	}
	if ep := byID[999]; !ep.Skip {
		t.Error("mix-show episode should be flagged Skip")
	}

	// ISRC settles a match outright — it identifies the exact recording,
	// so it short-circuits the fuzzy scoring where wrong edits get picked.
	// Nothing else asserted it survived the flattening. Note the capture's
	// only non-null isrc sits on the DJ mix, and a null one must arrive as
	// "" rather than the literal "null".
	if mix := byID[3041000]; mix.ISRC != "GBAAA1234567" {
		t.Errorf("ISRC = %q, want it carried through the flattening", mix.ISRC)
	}
	if track.ISRC != "" {
		t.Errorf("null ISRC = %q, want empty", track.ISRC)
	}
}

// TestListLikedTracks_RuntimeThresholdIsItsOwnFilter: the duration bound
// is a filter in its own right, not a side effect of mix=true. Kept as an
// inline payload because no recorded capture happens to contain a long
// non-mix track, and inventing one inside testdata/ would compromise the
// fixture's value as the API reference.
func TestListLikedTracks_RuntimeThresholdIsItsOwnFilter(t *testing.T) {
	const page = `[
		{"id":102,"track_id":2002,"up":true,"created_at":"2019-03-03T00:00:00-05:00",
		 "track":{"id":2002,"title":"A Fifty Minute Journey","display_artist":"X","length":3000,"mix":false}}
	]`
	srv := newPageServer(t, page)
	defer srv.Close()

	got, err := newClient(t, srv).ListLikedTracks(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("ListLikedTracks: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d tracks, want 1", len(got))
	}
	if !got[0].Skip {
		t.Error("a 3000s non-mix track should be flagged Skip on runtime")
	}
	if !strings.Contains(got[0].SkipReason, "runtime") {
		t.Errorf("SkipReason = %q, want the runtime threshold reason", got[0].SkipReason)
	}
}

// TestListLikedTracks_MalformedRecordDoesNotFailPage pins the package's
// central defensive-decoding promise. The API is unversioned and has
// changed shape before; one drifted field must cost one like, not the
// whole page — and with it, the whole sync.
func TestListLikedTracks_MalformedRecordDoesNotFailPage(t *testing.T) {
	const page = `[
		{"id":1,"track_id":11,"up":true,"created_at":"2026-01-01T00:00:00Z",
		 "track":{"id":11,"title":"Good One","display_artist":"A","length":200,"mix":false}},
		{"id":2,"track_id":12,"up":true,"created_at":"2026-01-02T00:00:00Z",
		 "track":{"id":12,"title":"Bad One","display_artist":"B","length":"200","mix":false}},
		{"id":3,"track_id":13,"up":true,"created_at":"2026-01-03T00:00:00Z",
		 "track":{"id":13,"title":"Good Two","display_artist":"C","length":210,"mix":false}}
	]`

	srv := newPageServer(t, page)
	defer srv.Close()

	var logged []string
	c := newClient(t, srv)
	c.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }

	got, err := c.ListLikedTracks(context.Background(), time.Time{})

	// The prefix survives — one drifted field costs one like, not the page.
	if len(got) != 2 {
		t.Fatalf("got %d tracks, want the 2 well-formed ones", len(got))
	}
	// But the pass is not clean. A dropped record reached no durable state
	// anywhere, so a caller that advanced its watermark over this page
	// would lose that like permanently and unrecoverably.
	if !errors.Is(err, difm.ErrDropped) {
		t.Errorf("err = %v, want ErrDropped so the caller holds its watermark", err)
	}
	if len(logged) == 0 {
		t.Error("skipping a malformed record should be reported, not silent")
	}
}

// TestListLikedTracks_FullySkippedPageIsNotTheEnd: a page can yield zero
// usable tracks and still be followed by more. Terminating on the track
// count rather than the row count read that as a clean, complete finish
// and silently truncated the history — and because the watermark filters
// at fetch time, every like beyond it became unreachable, including via
// resync.
func TestListLikedTracks_FullySkippedPageIsNotTheEnd(t *testing.T) {
	pages := map[string]string{
		"1": `[{"id":1,"track_id":11,"up":true,"created_at":"2026-01-03T00:00:00Z",
		       "track":{"id":11,"title":"Newest","display_artist":"A","length":200}}]`,
		// Every record here is filtered out, so the page yields no tracks.
		"2": `[{"id":2,"track_id":12,"up":false,"created_at":"2026-01-02T00:00:00Z",
		       "track":{"id":12,"title":"A Downvote","display_artist":"B","length":200}}]`,
		"3": `[{"id":3,"track_id":13,"up":true,"created_at":"2026-01-01T00:00:00Z",
		       "track":{"id":13,"title":"Beyond The Gap","display_artist":"C","length":210}}]`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, ok := pages[r.URL.Query().Get("page")]
		if !ok {
			body = "[]"
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	got, err := newClient(t, srv).ListLikedTracks(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("ListLikedTracks: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d tracks, want 2 — page 3 was not fetched", len(got))
	}
	var titles []string
	for _, tr := range got {
		titles = append(titles, tr.Title)
	}
	if !slices.Contains(titles, "Beyond The Gap") {
		t.Errorf("titles = %v, want the like that sits past the fully-filtered page", titles)
	}
}

// TestListLikedTracks_AllDroppedPageDoesNotEndPagination is the same
// hazard reached the other way: a page whose every record is malformed
// also yields no tracks, and must likewise not be read as the end.
func TestListLikedTracks_AllDroppedPageDoesNotEndPagination(t *testing.T) {
	pages := map[string]string{
		"1": `[{"id":1,"track_id":11,"up":true,"created_at":"2026-01-03T00:00:00Z",
		       "track":{"id":11,"title":"Newest","display_artist":"A","length":200}}]`,
		"2": `[{"id":2,"track_id":12,"up":true,"created_at":"2026-01-02T00:00:00Z",
		       "track":{"id":12,"title":"Drifted","display_artist":"B","length":"200"}}]`,
		"3": `[{"id":3,"track_id":13,"up":true,"created_at":"2026-01-01T00:00:00Z",
		       "track":{"id":13,"title":"Beyond The Gap","display_artist":"C","length":210}}]`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, ok := pages[r.URL.Query().Get("page")]
		if !ok {
			body = "[]"
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	got, err := newClient(t, srv).ListLikedTracks(context.Background(), time.Time{})
	if !errors.Is(err, difm.ErrDropped) {
		t.Errorf("err = %v, want ErrDropped", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d tracks, want 2 — pagination stopped at the bad page", len(got))
	}
}

// TestListLikedTracks_TruncationIsAnError: a server that never runs out
// of pages must not look like a clean, complete read. Reporting success
// here would let the caller advance its watermark past everything it
// never fetched, putting those likes permanently out of reach.
func TestListLikedTracks_TruncationIsAnError(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		_, _ = fmt.Fprintf(w, `[{"id":%d,"track_id":%d,"up":true,
			"created_at":"2026-01-01T00:00:00Z",
			"track":{"id":%d,"title":"T%d","display_artist":"A","length":200,"mix":false}}]`,
			page, page, page, page)
	}))
	defer srv.Close()

	got, err := newClient(t, srv).ListLikedTracks(context.Background(), time.Time{})
	if err == nil {
		t.Fatal("expected an error when pagination is truncated")
	}
	if !errors.Is(err, difm.ErrTruncated) {
		t.Errorf("err = %v, want ErrTruncated", err)
	}
	// The prefix is still returned so a caller can report it.
	if len(got) == 0 {
		t.Error("truncated result should still carry what was read")
	}
	if hits > 250 {
		t.Errorf("made %d requests; the page cap should bound this", hits)
	}
}

// TestListLikedTracks_RateLimitIsTyped: a 429 must be distinguishable
// from an empty result, or the caller records a wrong verdict for every
// track it was throttled on.
func TestListLikedTracks_RateLimitIsTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("slow down"))
	}))
	defer srv.Close()

	_, err := newClient(t, srv).ListLikedTracks(context.Background(), time.Time{})
	if !errors.Is(err, difm.ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	var rl *difm.RateLimitError
	if !errors.As(err, &rl) {
		t.Fatal("err should carry a *RateLimitError")
	}
	if rl.RetryAfter != 30*time.Second {
		t.Errorf("RetryAfter = %s, want 30s", rl.RetryAfter)
	}
}

// TestListLikedTracks_StopsOnceBelowWatermark: votes arrive newest-first,
// so a full page at or below the watermark means every later page is too.
// Walking the member's entire history on every 15-minute tick is a lot of
// traffic against a private API for zero new rows.
func TestListLikedTracks_StopsOnceBelowWatermark(t *testing.T) {
	var pages int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		_, _ = fmt.Fprintf(w, `[{"id":%d,"track_id":%d,"up":true,
			"created_at":"2020-01-01T00:00:00Z",
			"track":{"id":%d,"title":"Old %d","display_artist":"A","length":200,"mix":false}}]`,
			page, page, page, page)
	}))
	defer srv.Close()

	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	got, err := newClient(t, srv).ListLikedTracks(context.Background(), since)
	if err != nil {
		t.Fatalf("ListLikedTracks: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d tracks, want 0 (all older than the watermark)", len(got))
	}
	if pages != 1 {
		t.Errorf("fetched %d pages, want 1 — should stop at the first fully-old page", pages)
	}
}

func TestListLikedTracks_SinceFiltersOlderLikes(t *testing.T) {
	srv := newFixtureServer(t)
	defer srv.Close()

	since := time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC)
	got, err := newClient(t, srv).ListLikedTracks(context.Background(), since)
	if err != nil {
		t.Fatalf("ListLikedTracks: %v", err)
	}
	if len(got) != 1 || got[0].TrackID != 3041427 {
		t.Fatalf("got %d tracks, want only the one liked after %s", len(got), since)
	}
}

func TestListLikedTracks_UnauthorizedIsTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("Invalid Session"))
	}))
	defer srv.Close()

	c := difm.New("", "1")
	c.BaseURL = srv.URL
	_, err := c.ListLikedTracks(context.Background(), time.Time{})
	if err == nil {
		t.Fatal("expected an error on 403")
	}
	// Typed so the caller can distinguish "key revoked" (needs a human)
	// from a transient network failure (retry).
	if !errors.Is(err, difm.ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
}
