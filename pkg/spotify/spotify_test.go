package spotify_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mjrossi/difm-spotify-sync/pkg/spotify"
)

// newTestClient points a client at a stub server, bypassing OAuth: the
// token transport is orthogonal to the request shapes under test.
func newTestClient(t *testing.T, h http.Handler) *spotify.Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return spotify.NewClient(srv.Client(), srv.URL)
}

const twoTrackSearch = `{"tracks":{"items":[
  {"id":"wrongedit","name":"Air Race - Radio Edit","duration_ms":190000,
   "artists":[{"name":"DJ Rax"}],"external_ids":{"isrc":"AAA111"}},
  {"id":"rightone","name":"Air Race - Spiritchaser Remix","duration_ms":480000,
   "artists":[{"name":"DJ Rax"},{"name":"Spiritchaser"}],"external_ids":{"isrc":"BBB222"}}
]}}`

func TestSearchScoresAndSortsBestFirst(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/search") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, twoTrackSearch)
	}))

	got, err := c.Search(context.Background(), "DJ Rax", "Air Race (Spiritchaser Remix)", 480, "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d candidates, want 2", len(got))
	}

	// Spotify listed the radio edit first. Scoring must reorder it — that
	// reordering is the entire reason this client does not take result[0].
	if got[0].ID != "rightone" {
		t.Errorf("best = %q (%.3f), want rightone; Spotify's own order must not win",
			got[0].ID, got[0].Score)
	}
	if got[0].Score <= got[1].Score {
		t.Errorf("not sorted descending: %.3f then %.3f", got[0].Score, got[1].Score)
	}
	if got[0].ISRC != "BBB222" {
		t.Errorf("ISRC = %q, want BBB222", got[0].ISRC)
	}
	if got[0].DurationSec != 480 {
		t.Errorf("DurationSec = %d, want 480 (ms must be converted)", got[0].DurationSec)
	}
}

func TestSearchFallsBackToFreeformQuery(t *testing.T) {
	var queries []string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		queries = append(queries, q)
		// Field-scoped form finds nothing; freeform succeeds.
		if strings.Contains(q, "artist:") {
			_, _ = io.WriteString(w, `{"tracks":{"items":[]}}`)
			return
		}
		_, _ = io.WriteString(w, twoTrackSearch)
	}))

	got, err := c.Search(context.Background(), "DJ Rax", "Air Race (Spiritchaser Remix)", 480, "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(queries) != 2 {
		t.Fatalf("issued %d queries, want 2 (scoped then freeform)", len(queries))
	}
	if !strings.Contains(queries[0], "artist:") {
		t.Errorf("first query %q should be field-scoped", queries[0])
	}
	if len(got) == 0 {
		t.Error("fallback query returned no candidates")
	}
}

func TestSearchNoResultsIsNotAnError(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"tracks":{"items":[]}}`)
	}))
	got, err := c.Search(context.Background(), "Nobody", "Nothing", 100, "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil for no matches", got)
	}
}

// TestAddToPlaylistUsesItemsEndpoint pins the endpoint path. The
// deprecated /tracks path returns a bare 403 with no error body, which is
// extremely hard to diagnose in production — so it is asserted here.
func TestAddToPlaylistUsesItemsEndpoint(t *testing.T) {
	var paths []string
	var bodies []map[string]any

	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"snapshot_id":"abc"}`)
	}))

	if err := c.AddToPlaylist(context.Background(), "PL1", []string{"t1", "t2"}); err != nil {
		t.Fatalf("AddToPlaylist: %v", err)
	}
	if len(paths) != 1 {
		t.Fatalf("made %d requests, want 1", len(paths))
	}
	if paths[0] != "/playlists/PL1/items" {
		t.Errorf("path = %q, want /playlists/PL1/items (NOT the deprecated /tracks)", paths[0])
	}

	uris, _ := bodies[0]["uris"].([]any)
	if len(uris) != 2 || uris[0] != "spotify:track:t1" {
		t.Errorf("uris = %v, want fully-qualified spotify:track: URIs", uris)
	}
}

func TestAddToPlaylistBatchesAtOneHundred(t *testing.T) {
	var batches []int
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			URIs []string `json:"uris"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		batches = append(batches, len(body.URIs))
		w.WriteHeader(http.StatusCreated)
	}))

	ids := make([]string, 250)
	for i := range ids {
		ids[i] = "t"
	}
	if err := c.AddToPlaylist(context.Background(), "PL1", ids); err != nil {
		t.Fatalf("AddToPlaylist: %v", err)
	}
	want := []int{100, 100, 50}
	if len(batches) != len(want) {
		t.Fatalf("batches = %v, want %v", batches, want)
	}
	for i := range want {
		if batches[i] != want[i] {
			t.Errorf("batch %d = %d, want %d", i, batches[i], want[i])
		}
	}
}

func TestAddToPlaylistEmptyMakesNoRequest(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("no request should be made for an empty track list")
	}))
	if err := c.AddToPlaylist(context.Background(), "PL1", nil); err != nil {
		t.Fatalf("AddToPlaylist: %v", err)
	}
}

// Spotify often fails with a bare status and an unhelpful body; whatever
// body there is must reach the operator rather than being swallowed.
func TestErrorsSurfaceResponseBody(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"status":403,"message":"Forbidden"}}`)
	}))
	err := c.AddToPlaylist(context.Background(), "PL1", []string{"t1"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "Forbidden") {
		t.Errorf("err = %v, want it to carry the status and body", err)
	}
}

func TestPlaylistName(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("fields"); got != "name" {
			t.Errorf("fields = %q, want name (avoid fetching the whole playlist)", got)
		}
		_, _ = io.WriteString(w, `{"name":"DI.fm Favorites"}`)
	}))
	got, err := c.PlaylistName(context.Background(), "PL1")
	if err != nil {
		t.Fatalf("PlaylistName: %v", err)
	}
	if got != "DI.fm Favorites" {
		t.Errorf("PlaylistName = %q", got)
	}
}

func TestPlaylistTrackIDsPaginates(t *testing.T) {
	var offsets []string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offsets = append(offsets, r.URL.Query().Get("offset"))
		// Only the id field should be requested — the full item objects
		// are large and none of the rest is used.
		if f := r.URL.Query().Get("fields"); !strings.Contains(f, "item(id)") {
			t.Errorf("fields = %q, want it narrowed to item(id)", f)
		}
		switch r.URL.Query().Get("offset") {
		case "0":
			items := make([]string, 100)
			for i := range items {
				items[i] = `{"item":{"id":"t` + strconv.Itoa(i) + `"}}`
			}
			_, _ = io.WriteString(w, `{"total":102,"items":[`+strings.Join(items, ",")+`]}`)
		default:
			_, _ = io.WriteString(w, `{"total":102,"items":[{"item":{"id":"t100"}},{"item":{"id":"t101"}}]}`)
		}
	}))

	got, err := c.PlaylistTrackIDs(context.Background(), "PL1")
	if err != nil {
		t.Fatalf("PlaylistTrackIDs: %v", err)
	}
	if len(got) != 102 {
		t.Errorf("got %d ids, want 102 (pagination must follow through)", len(got))
	}
	if !got["t0"] || !got["t101"] {
		t.Error("missing ids from first or second page")
	}
	if len(offsets) != 2 || offsets[0] != "0" || offsets[1] != "100" {
		t.Errorf("offsets = %v, want [0 100]", offsets)
	}
}

// The migration renamed items[].track to items[].item. Decoding the old
// shape would silently yield an empty set — which would then duplicate
// every track on the next sync — so it is pinned here.
func TestPlaylistTrackIDsReadsItemNotTrack(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"total":1,"items":[{"track":{"id":"old-shape"}}]}`)
	}))
	got, err := c.PlaylistTrackIDs(context.Background(), "PL1")
	if err != nil {
		t.Fatalf("PlaylistTrackIDs: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v; the pre-migration `track` key must not be read", got)
	}
}

// TestSearch_RateLimitIsTyped: a 429 must not read as "Spotify has no
// such track". The sync engine branches on this to decide whether to
// back off or to record a permanent no-match verdict.
func TestSearch_RateLimitIsTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "12")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := spotify.NewClient(srv.Client(), srv.URL)
	_, err := c.Search(context.Background(), "A", "B", 100, "")
	if !errors.Is(err, spotify.ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	var rl *spotify.RateLimitError
	if !errors.As(err, &rl) {
		t.Fatal("err should carry a *RateLimitError")
	}
	if rl.RetryAfter != 12*time.Second {
		t.Errorf("RetryAfter = %s, want 12s", rl.RetryAfter)
	}
}

// TestSearch_UnauthorizedIsTyped: an expired grant needs a human, so it
// must be distinguishable from a rate limit, which just needs patience.
func TestSearch_UnauthorizedIsTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"status":401,"message":"expired"}}`)
	}))
	defer srv.Close()

	c := spotify.NewClient(srv.Client(), srv.URL)
	_, err := c.Search(context.Background(), "A", "B", 100, "")
	if !errors.Is(err, spotify.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if errors.Is(err, spotify.ErrRateLimited) {
		t.Error("a 401 must not also read as a rate limit")
	}
}

// TestSearch_ISRCShortCircuits: DI.fm supplies an ISRC for many tracks
// and it identifies the exact recording, so it settles the match without
// fuzzy scoring — and without the risk of picking the wrong edit.
func TestSearch_ISRCShortCircuits(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		queries = append(queries, q)
		if strings.HasPrefix(q, "isrc:") {
			_, _ = io.WriteString(w, `{"tracks":{"items":[{"id":"exact1","name":"Air Race",
				"duration_ms":480000,"artists":[{"name":"DJ Rax"}],
				"external_ids":{"isrc":"GBAAA1234567"}}]}}`)
			return
		}
		_, _ = io.WriteString(w, twoTrackSearch)
	}))
	defer srv.Close()

	c := spotify.NewClient(srv.Client(), srv.URL)
	got, err := c.Search(context.Background(), "DJ Rax", "Air Race", 480, "GBAAA1234567")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1 exact ISRC hit", len(got))
	}
	if got[0].ID != "exact1" {
		t.Errorf("ID = %q, want exact1", got[0].ID)
	}
	if got[0].Score != 1.0 {
		t.Errorf("Score = %v, want 1.0 for an ISRC match", got[0].Score)
	}
	if len(queries) != 1 {
		t.Errorf("issued %d queries (%v), want only the ISRC lookup", len(queries), queries)
	}
}

// TestSearch_ISRCMismatchFallsThrough: Spotify's isrc: filter is not
// always exact, so a code that comes back different must not be trusted.
func TestSearch_ISRCMismatchFallsThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Query().Get("q"), "isrc:") {
			_, _ = io.WriteString(w, `{"tracks":{"items":[{"id":"other","name":"Something Else",
				"duration_ms":200000,"artists":[{"name":"Whoever"}],
				"external_ids":{"isrc":"ZZZZZ9999999"}}]}}`)
			return
		}
		_, _ = io.WriteString(w, twoTrackSearch)
	}))
	defer srv.Close()

	c := spotify.NewClient(srv.Client(), srv.URL)
	got, err := c.Search(context.Background(), "DJ Rax", "Air Race (Spiritchaser Remix)", 480, "GBAAA1234567")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, g := range got {
		if g.ID == "other" {
			t.Fatal("a non-matching ISRC result was trusted as exact")
		}
	}
	if len(got) == 0 {
		t.Error("should have fallen through to the fuzzy path")
	}
}

// TestSearch_ScopedQueryFailureStillTriesFallback: the two-query design
// exists so a brittle scoped query has a backup. Returning early on its
// error defeated that.
func TestSearch_ScopedQueryFailureStillTriesFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Query().Get("q"), "artist:") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, twoTrackSearch)
	}))
	defer srv.Close()

	c := spotify.NewClient(srv.Client(), srv.URL)
	got, err := c.Search(context.Background(), "DJ Rax", "Air Race (Spiritchaser Remix)", 480, "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) == 0 {
		t.Error("scoped-query failure should fall through to the freeform query")
	}
}

// TestPlaylistTrackIDs_TerminatesOnMisbehavingServer: a server that keeps
// returning full pages previously spun forever, issuing requests as fast
// as the network allowed.
func TestPlaylistTrackIDs_TerminatesOnMisbehavingServer(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		var b strings.Builder
		b.WriteString(`{"total":100,"items":[`)
		for i := range 100 {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"item":{"id":"t%d"}}`, i)
		}
		b.WriteString(`]}`)
		_, _ = io.WriteString(w, b.String())
	}))
	defer srv.Close()

	c := spotify.NewClient(srv.Client(), srv.URL)
	ids, err := c.PlaylistTrackIDs(context.Background(), "pl1")
	if err != nil {
		t.Fatalf("PlaylistTrackIDs: %v", err)
	}
	// total=100 and the first page carried 100, so it must stop there.
	if len(ids) != 100 {
		t.Errorf("got %d ids, want 100", len(ids))
	}
	if requests != 1 {
		t.Errorf("made %d requests, want 1 — total should have stopped the walk", requests)
	}
}
