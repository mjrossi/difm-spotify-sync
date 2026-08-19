package syncer_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite" // same pure-Go driver the store uses

	"github.com/mjrossi/difm-spotify-sync/internal/store/sqlite"
	"github.com/mjrossi/difm-spotify-sync/internal/syncer"
	"github.com/mjrossi/difm-spotify-sync/pkg/difm"
	"github.com/mjrossi/difm-spotify-sync/pkg/spotify"
)

// The engine is tested against stub HTTP servers and a real SQLite store
// rather than mocked interfaces. The invariants worth protecting here are
// about *ordering* across those two boundaries — Spotify write, then
// ledger, then watermark — and a mock that cannot fail halfway would not
// exercise them.

// like is a compact description of one DI.fm liked track for a fixture.
type like struct {
	VoteID  int64
	TrackID int64
	Artist  string
	Title   string
	Length  int
	LikedAt time.Time
	Mix     bool
	Episode bool
}

func (l like) json() string {
	episode := "null"
	if l.Episode {
		episode = `{"id":1}`
	}
	return fmt.Sprintf(`{
      "id": %d, "track_id": %d, "channel_id": 1, "up": true,
      "created_at": %q, "episode": %s,
      "track": {"id": %d, "title": %q, "display_title": %q,
                "display_artist": %q, "length": %d, "mix": %t,
                "details_url": "https://www.di.fm/tracks/%d"}
    }`, l.VoteID, l.TrackID, l.LikedAt.Format(time.RFC3339), episode,
		l.TrackID, l.Title, l.Title, l.Artist, l.Length, l.Mix, l.TrackID)
}

// spotifyTrack is a stub search result.
type spotifyTrack struct {
	ID      string
	Artist  string
	Title   string
	Seconds int
}

func (s spotifyTrack) json() string {
	return fmt.Sprintf(`{"id":%q,"name":%q,"duration_ms":%d,"artists":[{"name":%q}],"external_ids":{}}`,
		s.ID, s.Title, s.Seconds*1000, s.Artist)
}

// harness wires an Engine to stub servers plus a real store, and records
// what the engine did on the Spotify side.
type harness struct {
	Engine *syncer.Engine
	Store  *sqlite.Store

	// Added accumulates every track URI the engine posted to the playlist.
	Added []string
	// SearchCount counts searches, to assert that filtered-out tracks are
	// never looked up and already-synced ones are not re-searched.
	SearchCount int

	// DBPath is the store's file, so a test can reach past the Store API
	// to inject a failure the API cannot produce (a trigger that aborts
	// a specific write).
	DBPath string

	// Knobs the tests flip.
	inPlaylist       []string
	searchResult     map[string][]spotifyTrack
	failAdd          bool
	failSearch       map[string]bool
	rateLimitSearch  bool
	failPlaylistRead bool

	// beforeSearch, when set, runs at the start of each search request.
	// Tests use it to interleave an event — a shutdown, say — into the
	// middle of a pass.
	beforeSearch func()

	// afterPlaylistRead, when set, runs after each playlist read with the
	// number of reads so far. Tests use it to have a track appear in the
	// playlist between the pass's reconciliation read and its add, which
	// is what a concurrent `review --approve` does.
	afterPlaylistRead func(reads int)
	playlistReads     int

	// rawRecords are appended verbatim to the DI.fm page, for payloads a
	// well-formed `like` cannot express — a drifted field, say.
	rawRecords []string
}

func newHarness(t *testing.T, likes []like) *harness {
	t.Helper()

	h := &harness{
		searchResult: map[string][]spotifyTrack{},
		failSearch:   map[string]bool{},
	}

	difmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "1" {
			_, _ = io.WriteString(w, "[]")
			return
		}
		parts := make([]string, 0, len(likes)+len(h.rawRecords))
		for _, l := range likes {
			parts = append(parts, l.json())
		}
		parts = append(parts, h.rawRecords...)
		_, _ = io.WriteString(w, "["+strings.Join(parts, ",")+"]")
	}))
	t.Cleanup(difmSrv.Close)

	spotifySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/search"):
			h.SearchCount++
			if h.beforeSearch != nil {
				h.beforeSearch()
			}
			if h.rateLimitSearch {
				w.Header().Set("Retry-After", "30")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			q := r.URL.Query().Get("q")
			for key, fail := range h.failSearch {
				if fail && strings.Contains(q, key) {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
			}
			var hits []spotifyTrack
			for key, res := range h.searchResult {
				if strings.Contains(q, key) {
					hits = res
					break
				}
			}
			items := make([]string, 0, len(hits))
			for _, hit := range hits {
				items = append(items, hit.json())
			}
			_, _ = io.WriteString(w, `{"tracks":{"items":[`+strings.Join(items, ",")+`]}}`)

		case strings.HasSuffix(r.URL.Path, "/items") && r.Method == http.MethodGet:
			if h.failPlaylistRead {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			items := make([]string, 0, len(h.inPlaylist))
			for _, id := range h.inPlaylist {
				items = append(items, fmt.Sprintf(`{"item":{"id":%q}}`, id))
			}
			_, _ = fmt.Fprintf(w, `{"total":%d,"items":[%s]}`, len(items), strings.Join(items, ","))
			h.playlistReads++
			if h.afterPlaylistRead != nil {
				h.afterPlaylistRead(h.playlistReads)
			}

		case strings.HasSuffix(r.URL.Path, "/items") && r.Method == http.MethodPost:
			if h.failAdd {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"error":{"status":403,"message":"Forbidden"}}`)
				return
			}
			var body struct {
				URIs []string `json:"uris"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			h.Added = append(h.Added, body.URIs...)
			// Reflect the write back into the playlist the way the real
			// API does, so a later pass's reconciliation read sees it.
			for _, uri := range body.URIs {
				h.inPlaylist = append(h.inPlaylist, strings.TrimPrefix(uri, "spotify:track:"))
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"snapshot_id":"snap"}`)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(spotifySrv.Close)

	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	h.DBPath = dbPath
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	account, err := store.EnsureAccount(context.Background(), "default", "1", "PL1")
	if err != nil {
		t.Fatalf("ensure account: %v", err)
	}

	dc := difm.New("key", "1")
	dc.BaseURL = difmSrv.URL

	h.Store = store
	h.Engine = &syncer.Engine{
		DiFM:       dc,
		Spotify:    spotify.NewClient(spotifySrv.Client(), spotifySrv.URL),
		Store:      store,
		Account:    account,
		PlaylistID: "PL1",
		Thresholds: syncer.Thresholds{Auto: 0.85, Review: 0.60},
		Log:        slog.New(slog.DiscardHandler),
	}
	return h
}

// reload re-reads the account so watermark assertions see persisted state
// rather than the engine's in-memory copy.
func (h *harness) reload(t *testing.T) sqlite.Account {
	t.Helper()
	acct, err := h.Store.GetAccount(context.Background(), "default")
	if err != nil {
		t.Fatalf("reload account: %v", err)
	}
	return acct
}

func (h *harness) ledgerCount(t *testing.T) int64 {
	t.Helper()
	n, err := h.Store.CountSynced(context.Background(), h.Engine.Account.ID)
	if err != nil {
		t.Fatalf("count synced: %v", err)
	}
	return n
}

func (h *harness) pending(t *testing.T) []sqlite.ReviewItem {
	t.Helper()
	items, err := h.Store.ListReview(context.Background(), h.Engine.Account.ID, "pending", 100)
	if err != nil {
		t.Fatalf("list review: %v", err)
	}
	return items
}

// exec runs a statement against the store's file on a separate
// connection. Tests use it to install a trigger that makes one specific
// write fail — a failure mode the Store API cannot produce on demand,
// but which the invariants have to survive.
func (h *harness) exec(t *testing.T, stmt string) {
	t.Helper()
	db, err := sql.Open("sqlite", h.DBPath)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(stmt); err != nil {
		t.Fatalf("exec %q: %v", stmt, err)
	}
}
