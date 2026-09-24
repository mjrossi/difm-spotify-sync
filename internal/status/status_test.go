package status_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
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
	return slog.New(slog.DiscardHandler)
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

// recordFailedRun is recordRun for a pass that ended with a kind.
func recordFailedRun(t *testing.T, s *sqlite.Store, accountID int64, age time.Duration, kind sqlite.RunErrorKind, runErr error) {
	t.Helper()
	ctx := context.Background()
	at := time.Now().Add(-age)
	s.SetClock(func() time.Time { return at })
	defer s.SetClock(time.Now)

	id, err := s.StartRun(ctx, accountID, false)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if err := s.FinishRun(ctx, id, sqlite.RunStats{Err: runErr, Kind: kind}); err != nil {
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

			rep, err := status.Build(context.Background(), s, testLabel, testMaxAge, 10, "", "")
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

	// A real version string, so its presence in the body is a positive
	// assertion rather than one that would pass vacuously with "".
	srv := httptest.NewServer(status.Handler(s, testLabel, testMaxAge, "v9.9.9-test", "", discardLogger()))
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
	// Version, last_success_at and consecutive_failures are new fields on
	// the same struct the secret checks above cover — asserting on them
	// here is what keeps a future field from being added to Report
	// without this test being looked at.
	if !strings.Contains(string(body), `"version":"v9.9.9-test"`) {
		t.Errorf("body does not carry the version: %s", body)
	}
	if !strings.Contains(string(body), `"last_success_at"`) {
		t.Errorf("body does not carry last_success_at: %s", body)
	}
	if !strings.Contains(string(body), `"consecutive_failures":0`) {
		t.Errorf("body does not carry consecutive_failures: %s", body)
	}
}

// errWithMemberID is what a DI.fm transport failure used to look like by
// the time it reached sync_runs.error: net/http returns *url.Error, whose
// Error() embeds the request URL, and the member id is a path segment of
// every track_votes request.
var errWithMemberID = errors.New(
	`sync pass incomplete: difm: get track_votes page 1: Get ` +
		`"https://api.audioaddict.com/v1/di/members/` + testMemberID +
		`/track_votes?page=1": dial tcp: lookup api.audioaddict.com: no such host`)

// TestEndpointsCarryNoSecretsFromAFailedRun covers the path the test
// above cannot: a pass that *failed*.
//
// The fixture matters more than the assertions here. TestReportCarriesNoSecrets
// records a clean run, so sync_runs.error is empty and every leak channel
// it might have exercised is dormant — it passed just as happily when the
// report embedded the store struct verbatim. Recorded error text is
// attacker-uncontrolled but author-unreviewed: it is assembled from
// whatever failed, and nothing between there and the encoder looks at it.
//
// Both endpoints are checked because they leak independently. /status.json
// carried it in runs[].error; /healthz never renders runs at all and
// carried the same id in its plain-text reason, via describe(). Fixing
// only the first leaves the second serving it to whatever polls the probe.
func TestEndpointsCarryNoSecretsFromAFailedRun(t *testing.T) {
	s, account := newStore(t)
	// Newest run failed, and nothing clean behind it — so health() has to
	// fall through to describe(), which is the /healthz leak channel.
	//
	// A kinded failure, because the kind path is the one branch of
	// describe() that says more than the generic text — so it is the one
	// that could leak if a later edit interpolated the run.
	recordFailedRun(t, s, account.ID, time.Minute, sqlite.KindDiFMUnauthorized, errWithMemberID)

	srv := httptest.NewServer(status.Handler(s, testLabel, testMaxAge, "", "", discardLogger()))
	defer srv.Close()

	for _, path := range []string{"/status.json", "/healthz"} {
		t.Run(path, func(t *testing.T) {
			resp, err := http.Get(srv.URL + path)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if strings.Contains(string(body), testMemberID) {
				t.Errorf("%s leaked the DI.fm member id from a failed run: %s", path, body)
			}
			if strings.Contains(string(body), refreshToken) {
				t.Errorf("%s leaked the Spotify refresh token: %s", path, body)
			}
			// The endpoint must still say something. A body that reported
			// nothing would pass the checks above for the wrong reason.
			if len(strings.TrimSpace(string(body))) == 0 {
				t.Fatal("empty body")
			}
		})
	}
}

// TestReasonNamesTheFailureKind: the endpoints may not serve error text,
// so before the kind existed every failed pass produced the same reason
// and the operator had to exec into the container to learn whether the
// first move was "re-extract the DI.fm key" or "click the consent URL".
func TestReasonNamesTheFailureKind(t *testing.T) {
	for _, tc := range []struct {
		name       string
		kind       sqlite.RunErrorKind
		wantReason string // substring
	}{
		{"spotify grant revoked", sqlite.KindSpotifyGrantRevoked, "grant revoked"},
		{"difm unauthorized", sqlite.KindDiFMUnauthorized, "DI.fm API key rejected"},
		{"rate limited", sqlite.KindRateLimited, "rate limited"},
		{"incomplete", sqlite.KindIncomplete, "newest run errored"},
		{"error", sqlite.KindError, "newest run errored"},
		{"pre-0002 row", "", "newest run errored"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, account := newStore(t)
			recordFailedRun(t, s, account.ID, time.Minute, tc.kind, errWithMemberID)

			rep, err := status.Build(context.Background(), s, testLabel, testMaxAge, 0, "", "")
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if rep.Healthy {
				t.Fatal("Healthy = true over a failed newest run")
			}
			if !strings.Contains(rep.Reason, tc.wantReason) {
				t.Errorf("Reason = %q, want it to contain %q", rep.Reason, tc.wantReason)
			}
			if strings.Contains(rep.Reason, testMemberID) {
				t.Errorf("Reason leaked the member id: %q", rep.Reason)
			}
			if len(rep.Runs) == 0 || rep.Runs[0].ErrorKind != string(tc.kind) {
				t.Errorf("Runs[0].ErrorKind = %q, want %q", rep.Runs[0].ErrorKind, tc.kind)
			}
		})
	}
}

// TestStatusJSONDropsAnUnknownKind is the read-side half of the guard.
// FinishRun refuses to write a kind this package did not define, but the
// status endpoints answer from whatever database they are handed — a
// restored or hand-edited one included — so a row that got an unknown
// kind past the write side some other way must still not be published
// verbatim.
func TestStatusJSONDropsAnUnknownKind(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	ctx := context.Background()
	s, err := sqlite.Open(dbPath)
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
	recordFailedRun(t, s, account.ID, time.Minute, sqlite.KindError, errWithMemberID)

	// A row this process did not write through FinishRun: a hand edit, or
	// what a restored database could carry. The Store API has no way to
	// produce this on demand, so a raw connection on the same file is the
	// only way to get it there.
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec(`UPDATE sync_runs SET error_kind = 'SMUGGLED-4242'`); err != nil {
		t.Fatalf("UPDATE sync_runs: %v", err)
	}

	srv := httptest.NewServer(status.Handler(s, testLabel, testMaxAge, "", "", discardLogger()))
	defer srv.Close()

	for _, path := range []string{"/status.json", "/healthz"} {
		t.Run(path, func(t *testing.T) {
			resp, err := http.Get(srv.URL + path)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if strings.Contains(string(body), "SMUGGLED") {
				t.Errorf("%s published an unknown error_kind verbatim: %s", path, body)
			}
		})
	}
}

// TestCLIKeepsTheErrorText is the other half of the fix. Redacting the
// endpoints is only correct if the text is still reachable somewhere —
// otherwise a failing deployment becomes undiagnosable, which is a worse
// outcome than the disclosure. status.Run keeps it as an untagged field
// for the CLI table.
func TestCLIKeepsTheErrorText(t *testing.T) {
	s, account := newStore(t)
	recordRun(t, s, account.ID, time.Minute, false, errPass)

	rep, err := status.Build(context.Background(), s, testLabel, testMaxAge, status.DefaultRunLimit, "", "")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(rep.Runs) != 1 {
		t.Fatalf("got %d runs, want 1", len(rep.Runs))
	}
	if !strings.Contains(rep.Runs[0].Error, "search failed") {
		t.Errorf("Run.Error = %q, want it to carry the recorded text", rep.Runs[0].Error)
	}
	if !rep.Runs[0].Failed {
		t.Error("Run.Failed = false, want true for a run that recorded an error")
	}

	// ...and the same value must not survive the JSON encoder.
	blob, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(blob), "search failed") {
		t.Errorf("encoded report carries the error text: %s", blob)
	}
	if !strings.Contains(string(blob), `"failed":true`) {
		t.Errorf("encoded report drops the failed flag, leaving no signal at all: %s", blob)
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

			srv := httptest.NewServer(status.Handler(s, testLabel, testMaxAge, "", "", discardLogger()))
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

	srv := httptest.NewServer(status.Handler(s, testLabel, testMaxAge, "", "", discardLogger()))
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

	srv := httptest.NewServer(status.Handler(s, testLabel, testMaxAge, "", "", discardLogger()))
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

	rep, err := status.Build(context.Background(), s, testLabel, testMaxAge, status.DefaultRunLimit, "", "")
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

// The verdict must not depend on how many runs the caller asked to see.
//
// It did: health() scans for the newest *qualifying* row, so a small
// runLimit silently narrowed the search as well as the listing. /healthz
// passed 1, and because the engine opens a sync_runs row when a pass
// starts and closes it when it ends, the only visible row for the whole
// duration of every pass was the in-flight one — so /healthz reported 503
// through every sync while `status --check` reported ok.
func TestHealthIgnoresRunLimit(t *testing.T) {
	ctx := context.Background()
	s, account := newStore(t)

	// A clean pass, then a pass currently in flight on top of it.
	recordRun(t, s, account.ID, time.Minute, false, nil)
	if _, err := s.StartRun(ctx, account.ID, false); err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	for _, limit := range []int{1, 2, 5, 50} {
		rep, err := status.Build(ctx, s, testLabel, testMaxAge, limit, "", "")
		if err != nil {
			t.Fatalf("Build(limit=%d): %v", limit, err)
		}
		if !rep.Healthy {
			t.Errorf("limit=%d: Healthy = false (%q), want true — an in-flight pass "+
				"must not hide the clean pass behind it", limit, rep.Reason)
		}
		if len(rep.Runs) > limit {
			t.Errorf("limit=%d: reported %d runs, want at most %d", limit, len(rep.Runs), limit)
		}
	}
}

// The in-flight row is still reported, it just does not decide the verdict.
func TestInFlightRunIsStillListed(t *testing.T) {
	ctx := context.Background()
	s, account := newStore(t)
	recordRun(t, s, account.ID, time.Minute, false, nil)
	if _, err := s.StartRun(ctx, account.ID, false); err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	rep, err := status.Build(ctx, s, testLabel, testMaxAge, status.DefaultRunLimit, "", "")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(rep.Runs) != 2 {
		t.Fatalf("len(Runs) = %d, want 2", len(rep.Runs))
	}
	if rep.Runs[0].FinishedAt != "" {
		t.Error("Runs[0] should be the in-flight pass (newest first)")
	}
}

// TestUnauthorizedAccountReportsWhyItIsUnhealthy covers the state the
// in-daemon consent flow creates and the old code could not: an account
// row exists, the container is up and serving, but consent has never been
// given.
//
// Before this, such a deployment reported "no sync pass has run yet",
// which sends an operator to the logs hunting a failure when what is
// actually needed is one click. The clause wins over the run-based reason
// deliberately, so a container that has also stacked up failed passes
// still names the cause rather than a symptom.
func TestUnauthorizedAccountReportsWhyItIsUnhealthy(t *testing.T) {
	ctx := context.Background()
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "unauth.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	account, err := s.EnsureAccount(ctx, testLabel, testMemberID, testPlaylist)
	if err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}
	// A clean, recent pass — which on its own would report healthy. It
	// cannot, because there is no token for it to have used.
	recordRun(t, s, account.ID, time.Minute, false, nil)

	rep, err := status.Build(ctx, s, testLabel, testMaxAge, 0, "", "")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if rep.Authorized {
		t.Error("Authorized = true for an account with no refresh token")
	}
	if rep.Healthy {
		t.Error("Healthy = true for an account that has never given consent")
	}
	if !strings.Contains(rep.Reason, "consent") {
		t.Errorf("Reason = %q, want it to name the missing consent", rep.Reason)
	}

	// And the positive case, so the clause cannot simply be always-on.
	if err := s.SetSpotifyRefreshToken(ctx, account.ID, refreshToken); err != nil {
		t.Fatalf("SetSpotifyRefreshToken: %v", err)
	}
	rep, err = status.Build(ctx, s, testLabel, testMaxAge, 0, "", "")
	if err != nil {
		t.Fatalf("Build after consent: %v", err)
	}
	if !rep.Authorized || !rep.Healthy {
		t.Errorf("after consent: Authorized=%v Healthy=%v (%s), want both true",
			rep.Authorized, rep.Healthy, rep.Reason)
	}
}

// The three numbers a probe wants next to the boolean: when the last
// success was, how many failures have stacked since, and which binary
// is answering.
func TestReportCarriesSuccessTimeAndFailureCount(t *testing.T) {
	ctx := context.Background()
	s, account := newStore(t)
	recordRun(t, s, account.ID, 40*time.Minute, false, nil) // clean
	recordFailedRun(t, s, account.ID, 30*time.Minute, sqlite.KindError, errPass)
	recordRun(t, s, account.ID, 20*time.Minute, true, errPass) // a dry run that swallowed a failure: still not counted
	recordFailedRun(t, s, account.ID, 10*time.Minute, sqlite.KindRateLimited, errPass)
	// An in-flight row: started, never finished. Not counted either.
	if _, err := s.StartRun(ctx, account.ID, false); err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	rep, err := status.Build(ctx, s, testLabel, testMaxAge, 0, "v9.9.9-test", "")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if rep.Version != "v9.9.9-test" {
		t.Errorf("Version = %q", rep.Version)
	}
	if rep.ConsecutiveFailures != 2 {
		t.Errorf("ConsecutiveFailures = %d, want 2 (dry run and in-flight row excluded)", rep.ConsecutiveFailures)
	}
	if rep.LastSuccessAt == "" {
		t.Fatal("LastSuccessAt empty with a clean run recorded")
	}
	at, err := time.Parse(sqlite.TimeFormat, rep.LastSuccessAt)
	if err != nil {
		t.Fatalf("LastSuccessAt = %q, not %s", rep.LastSuccessAt, sqlite.TimeFormat)
	}
	if age := time.Since(at); age < 39*time.Minute || age > 41*time.Minute {
		t.Errorf("LastSuccessAt is %s old, want ~40m", age)
	}
	if !rep.Healthy {
		t.Error("Healthy = false with a 40m-old clean run and a 45m window")
	}
}

func TestConsecutiveFailuresIsZeroWhenTheNewestRunIsClean(t *testing.T) {
	s, account := newStore(t)
	recordFailedRun(t, s, account.ID, 20*time.Minute, sqlite.KindError, errPass)
	recordRun(t, s, account.ID, 10*time.Minute, false, nil)
	rep, err := status.Build(context.Background(), s, testLabel, testMaxAge, 0, "", "")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if rep.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures = %d, want 0", rep.ConsecutiveFailures)
	}
}

func TestConsecutiveFailuresWithNoCleanRunCountsTheWindow(t *testing.T) {
	s, account := newStore(t)
	for i := 1; i <= 3; i++ {
		recordFailedRun(t, s, account.ID, time.Duration(i)*time.Minute, sqlite.KindError, errPass)
	}
	rep, err := status.Build(context.Background(), s, testLabel, testMaxAge, 0, "", "")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if rep.ConsecutiveFailures != 3 || rep.LastSuccessAt != "" {
		t.Errorf("ConsecutiveFailures = %d, LastSuccessAt = %q; want 3 and empty", rep.ConsecutiveFailures, rep.LastSuccessAt)
	}
}

// The verdict and the count must use the same fixed-size window
// regardless of how many rows the caller asks to see, in either
// direction. Narrowing was closed once already (TestHealthIgnoresRunLimit);
// this covers widening: a --limit above HealthScanLimit must not pull a
// clean run that has fallen out of the scan window back into the verdict.
func TestScanWindowIsFixedInBothDirections(t *testing.T) {
	s, account := newStore(t)
	// The clean run is older than all HealthScanLimit+5 failures stacked
	// on top of it, so it sits just past the fixed window.
	recordRun(t, s, account.ID, 30*time.Minute, false, nil)
	for i := 1; i <= status.HealthScanLimit+5; i++ {
		recordFailedRun(t, s, account.ID, time.Duration(i)*time.Minute, sqlite.KindError, errPass)
	}

	for _, limit := range []int{5, status.HealthScanLimit, 50} {
		rep, err := status.Build(context.Background(), s, testLabel, testMaxAge, limit, "", "")
		if err != nil {
			t.Fatalf("Build(limit=%d): %v", limit, err)
		}
		if rep.Healthy {
			t.Errorf("limit=%d: Healthy = true, want false — the clean run has fallen out of the window", limit)
		}
		if rep.ConsecutiveFailures != status.HealthScanLimit {
			t.Errorf("limit=%d: ConsecutiveFailures = %d, want %d", limit, rep.ConsecutiveFailures, status.HealthScanLimit)
		}
		if rep.LastSuccessAt != "" {
			t.Errorf("limit=%d: LastSuccessAt = %q, want empty", limit, rep.LastSuccessAt)
		}
	}
}

// TestLastBackupAtReadsTheNewestSnapshot: the report names the newest
// snapshot's date, read straight from the directory listing rather than a
// store column — there is no second place for it to disagree with what
// backup.go actually wrote. Snapshot names are ISO-dated
// (difmsync-YYYY-MM-DD.db), so a lexical sort over the filtered names is a
// chronological one.
func TestLastBackupAtReadsTheNewestSnapshot(t *testing.T) {
	s, account := newStore(t)
	recordRun(t, s, account.ID, time.Minute, false, nil)

	dir := t.TempDir()
	for _, name := range []string{
		"difmsync-2026-01-05.db",
		"difmsync-2026-01-06.db",
		"difmsync-2026-01-04.db",
		"not-a-snapshot.txt", // must be ignored rather than sorted in
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile(%s): %v", name, err)
		}
	}

	rep, err := status.Build(context.Background(), s, testLabel, testMaxAge, 0, "", dir)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if rep.LastBackupAt != "2026-01-06" {
		t.Errorf("LastBackupAt = %q, want 2026-01-06", rep.LastBackupAt)
	}
}

// TestLastBackupAtIgnoresNamesThatAreNotDates: a name matching
// difmsync-*.db whose middle is not a date — a manual `--to=` backup, a
// half-written file — must not be read as the newest snapshot. Today
// "zzz" sorts lexically above every real ISO date, so before this fix the
// report published exactly that string: an unvalidated filename fragment,
// on an endpoint served unauthenticated to the LAN.
func TestLastBackupAtIgnoresNamesThatAreNotDates(t *testing.T) {
	s, account := newStore(t)
	recordRun(t, s, account.ID, time.Minute, false, nil)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "difmsync-zzz.db"), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	rep, err := status.Build(context.Background(), s, testLabel, testMaxAge, 0, "", dir)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if rep.LastBackupAt != "" {
		t.Errorf("LastBackupAt = %q, want empty — %q is not a date", rep.LastBackupAt, "difmsync-zzz.db")
	}
}

// TestLastBackupAtEmptyWhenNoSnapshot covers the directory states that are
// not an error but still have nothing to report: unset, empty and missing.
func TestLastBackupAtEmptyWhenNoSnapshot(t *testing.T) {
	s, account := newStore(t)
	recordRun(t, s, account.ID, time.Minute, false, nil)

	tests := []struct {
		name string
		dir  string
	}{
		{"unset", ""},
		{"empty directory", t.TempDir()},
		{"missing directory", filepath.Join(t.TempDir(), "does-not-exist")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rep, err := status.Build(context.Background(), s, testLabel, testMaxAge, 0, "", tt.dir)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if rep.LastBackupAt != "" {
				t.Errorf("LastBackupAt = %q, want empty", rep.LastBackupAt)
			}
		})
	}
}

// TestLastBackupAtUnreadableDirDoesNotFailReport: a probe that 500s
// because the backup directory is missing or unreadable is worse than a
// report that just says nothing has been backed up yet.
func TestLastBackupAtUnreadableDirDoesNotFailReport(t *testing.T) {
	s, account := newStore(t)
	recordRun(t, s, account.ID, time.Minute, false, nil)

	parent := t.TempDir()
	dir := filepath.Join(parent, "backups")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "difmsync-2026-01-06.db"), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	// Restore permissions so t.TempDir()'s own cleanup can remove it.
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	rep, err := status.Build(context.Background(), s, testLabel, testMaxAge, 0, "", dir)
	if err != nil {
		t.Fatalf("Build: %v, want no error over an unreadable backup directory", err)
	}
	if rep.LastBackupAt != "" {
		t.Errorf("LastBackupAt = %q, want empty for an unreadable directory", rep.LastBackupAt)
	}
}

// TestLastBackupAtDoesNotAffectHealth: a missing or unreadable backup
// directory is not evidence that syncing has stopped, and must not change
// the health verdict computed from sync_runs.
func TestLastBackupAtDoesNotAffectHealth(t *testing.T) {
	dirs := map[string]string{
		"unset":     "",
		"missing":   filepath.Join(t.TempDir(), "does-not-exist"),
		"empty":     t.TempDir(),
		"populated": t.TempDir(),
	}
	if err := os.WriteFile(filepath.Join(dirs["populated"], "difmsync-2026-01-06.db"), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	for _, tc := range []struct {
		name       string
		age        time.Duration
		wantHealth bool
	}{
		{"fresh clean run", time.Minute, true},
		{"stale clean run", 3 * time.Hour, false},
	} {
		for dirName, dir := range dirs {
			t.Run(tc.name+"/"+dirName, func(t *testing.T) {
				s, account := newStore(t)
				recordRun(t, s, account.ID, tc.age, false, nil)

				rep, err := status.Build(context.Background(), s, testLabel, testMaxAge, 0, "", dir)
				if err != nil {
					t.Fatalf("Build: %v", err)
				}
				if rep.Healthy != tc.wantHealth {
					t.Errorf("Healthy = %v, want %v (backup dir %q must not affect the verdict)",
						rep.Healthy, tc.wantHealth, dirName)
				}
			})
		}
	}
}

// TestReportCarriesLastBackupAt is the JSON-facing sibling of
// TestLastBackupAtReadsTheNewestSnapshot: it belongs on the same struct
// the secret checks above cover, so its presence in the encoded body gets
// its own assertion rather than being inferred from Build's return value.
func TestReportCarriesLastBackupAt(t *testing.T) {
	s, account := newStore(t)
	recordRun(t, s, account.ID, time.Minute, false, nil)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "difmsync-2026-01-06.db"), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	srv := httptest.NewServer(status.Handler(s, testLabel, testMaxAge, "", dir, discardLogger()))
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
	if !strings.Contains(string(body), `"last_backup_at":"2026-01-06"`) {
		t.Errorf("body does not carry last_backup_at: %s", body)
	}
}
