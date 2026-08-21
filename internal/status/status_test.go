package status_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/mjrossi/difm-spotify-sync/internal/status"
	"github.com/mjrossi/difm-spotify-sync/internal/store/sqlite"
)

const (
	testLabel    = "default"
	testPlaylist = "playlist123"
	testMemberID = "4242"
	refreshToken = "AQC-super-secret-refresh-token"
	testMaxAge   = 45 * time.Minute
)

// errPass stands in for what the engine records when a pass swallows
// something: ErrPassIncomplete wrapping the first failure.
var errPass = errors.New("sync pass incomplete: search failed")

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newStore(t *testing.T) (*sqlite.Store, sqlite.Account) {
	t.Helper()
	ctx := context.Background()
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	account, err := s.EnsureAccount(ctx, testLabel, testMemberID, testPlaylist)
	if err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}
	if err := s.SetSpotifyRefreshToken(ctx, account.ID, refreshToken); err != nil {
		t.Fatalf("SetSpotifyRefreshToken: %v", err)
	}
	return s, account
}

// recordRun writes one finished sync_runs row, backdated by age. The
// store's clock is what StartRun and FinishRun stamp with, so moving it
// is how a run is aged without sleeping.
func recordRun(t *testing.T, s *sqlite.Store, accountID int64, age time.Duration, dryRun bool, runErr error) {
	t.Helper()
	ctx := context.Background()
	at := time.Now().Add(-age)
	s.SetClock(func() time.Time { return at })
	defer s.SetClock(time.Now)

	id, err := s.StartRun(ctx, accountID, dryRun)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if err := s.FinishRun(ctx, id, sqlite.RunStats{Added: 1, Err: runErr}); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
}

// The freshness rule is the whole reason this package exists: the old
// healthcheck ran `status`, which only failed when the account row was
// missing, so a sync that had been broken for a week still reported
// healthy. Each case here is a way that can happen.
func TestHealth(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(t *testing.T, s *sqlite.Store, accountID int64)
		wantHealth bool
		wantReason string // substring
	}{
		{
			name:       "no runs at all",
			setup:      func(*testing.T, *sqlite.Store, int64) {},
			wantHealth: false,
			wantReason: "no sync pass has run yet",
		},
		{
			name: "fresh clean run",
			setup: func(t *testing.T, s *sqlite.Store, id int64) {
				recordRun(t, s, id, time.Minute, false, nil)
			},
			wantHealth: true,
		},
		{
			name: "clean run older than max-age",
			setup: func(t *testing.T, s *sqlite.Store, id int64) {
				recordRun(t, s, id, 3*time.Hour, false, nil)
			},
			wantHealth: false,
			wantReason: "last clean pass finished",
		},
		{
			// The engine records the error and holds the watermark back.
			// Reporting this healthy would report green on precisely the
			// case the watermark logic exists to survive.
			name: "newest run errored",
			setup: func(t *testing.T, s *sqlite.Store, id int64) {
				recordRun(t, s, id, time.Minute, false, errPass)
			},
			wantHealth: false,
			wantReason: "newest run errored",
		},
		{
			// The deployed loop never dry-runs, so a stale `just dry-run`
			// must not stand in for a real pass.
			name: "only dry runs",
			setup: func(t *testing.T, s *sqlite.Store, id int64) {
				recordRun(t, s, id, time.Minute, true, nil)
			},
			wantHealth: false,
			wantReason: "newest run was a dry run",
		},
		{
			// A fresh failure does not erase an older success, but the
			// older success is what decides health.
			name: "errored run followed by an older clean one",
			setup: func(t *testing.T, s *sqlite.Store, id int64) {
				recordRun(t, s, id, 5*time.Minute, false, nil)
				recordRun(t, s, id, time.Minute, false, errPass)
			},
			wantHealth: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, account := newStore(t)
			tt.setup(t, s, account.ID)

			rep, err := status.Build(context.Background(), s, testLabel, testMaxAge, 10)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if rep.Healthy != tt.wantHealth {
				t.Errorf("Healthy = %v (reason %q), want %v", rep.Healthy, rep.Reason, tt.wantHealth)
			}
			if tt.wantReason != "" && !strings.Contains(rep.Reason, tt.wantReason) {
				t.Errorf("Reason = %q, want it to contain %q", rep.Reason, tt.wantReason)
			}
			if tt.wantHealth && rep.Reason != "" {
				t.Errorf("Reason = %q, want empty when healthy", rep.Reason)
			}
		})
	}
}

// The report is assembled field by field from typed accessors so that the
// refresh token — which sits on the same accounts row as the label and
// the watermark — cannot reach the encoder. That is a claim about code
// that will be edited later, so it gets a test rather than a comment.
func TestReportCarriesNoSecrets(t *testing.T) {
	s, account := newStore(t)
	recordRun(t, s, account.ID, time.Minute, false, nil)

	srv := httptest.NewServer(status.Handler(s, testLabel, testMaxAge, discardLogger()))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/status.json")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if strings.Contains(string(body), refreshToken) {
		t.Error("/status.json leaked the Spotify refresh token")
	}
	// The member id is not a credential on its own, but it is half of the
	// DI.fm capture and there is no reason for a status page to carry it.
	if strings.Contains(string(body), testMemberID) {
		t.Error("/status.json leaked the DI.fm member id")
	}
	// Guard against the test passing because the body was empty.
	if !strings.Contains(string(body), testPlaylist) {
		t.Fatalf("body does not look like a report: %s", body)
	}
}

func TestHealthzStatusCodes(t *testing.T) {
	tests := []struct {
		name     string
		age      time.Duration
		wantCode int
	}{
		{"fresh pass", time.Minute, http.StatusOK},
		{"stale pass", 3 * time.Hour, http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, account := newStore(t)
			recordRun(t, s, account.ID, tt.age, false, nil)

			srv := httptest.NewServer(status.Handler(s, testLabel, testMaxAge, discardLogger()))
			defer srv.Close()

			resp, err := http.Get(srv.URL + "/healthz")
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tt.wantCode {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, want %d (body %q)", resp.StatusCode, tt.wantCode, body)
			}
		})
	}
}

// An account that does not exist yet is the pre-auth state, and it must
// read as unhealthy rather than as a crash. The long start_period in
// compose.yaml covers the window; reporting 200 here would invert the
// signal on precisely the day someone is watching it.
func TestHealthzBeforeAuth(t *testing.T) {
	ctx := context.Background()
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	srv := httptest.NewServer(status.Handler(s, testLabel, testMaxAge, discardLogger()))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 before auth has run", resp.StatusCode)
	}
}

// /status.json reports state, so "healthy": false is a 200 answer. Only
// /healthz encodes the verdict in the status code.
func TestStatusJSONIs200WhenUnhealthy(t *testing.T) {
	s, account := newStore(t)
	recordRun(t, s, account.ID, 3*time.Hour, false, nil)

	srv := httptest.NewServer(status.Handler(s, testLabel, testMaxAge, discardLogger()))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/status.json")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var rep status.Report
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rep.Healthy {
		t.Error("Healthy = true, want false for a stale pass")
	}
	if rep.Reason == "" {
		t.Error("Reason is empty on an unhealthy report")
	}
}

// The runs table is what `status`'s usage string has always promised and
// never delivered, so the report must actually carry it.
func TestReportCarriesRuns(t *testing.T) {
	s, account := newStore(t)
	recordRun(t, s, account.ID, 2*time.Minute, false, nil)
	recordRun(t, s, account.ID, time.Minute, false, errPass)

	rep, err := status.Build(context.Background(), s, testLabel, testMaxAge, status.DefaultRunLimit)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(rep.Runs) != 2 {
		t.Fatalf("len(Runs) = %d, want 2", len(rep.Runs))
	}
	// ListRuns is newest-first, which is what health() depends on.
	if rep.Runs[0].Error == "" {
		t.Error("Runs[0] should be the newest (errored) run")
	}
}
