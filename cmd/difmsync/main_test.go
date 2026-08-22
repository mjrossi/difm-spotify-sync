package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mjrossi/difm-spotify-sync/internal/store/sqlite"
)

// The CLI layer had no tests, and three of its bugs lived precisely
// there: a listener bound where the callback could not reach it, a
// hard-coded callback path that ignored the configured redirect URL, and
// `resync --forget` quietly dropping an explicit `--all`.

func TestCallbackTarget(t *testing.T) {
	tests := []struct {
		name      string
		redirect  string
		bind      string
		wantAddr  string
		wantPath  string
		wantError bool
	}{
		{
			name:     "default loopback redirect",
			redirect: "http://127.0.0.1:8888/callback",
			wantAddr: "127.0.0.1:8888", wantPath: "/callback",
		},
		{
			// The path is the operator's to choose — it has to match what
			// they registered in the Spotify dashboard. Serving /callback
			// regardless meant a 404 and a five-minute hang.
			name:     "a custom path is honored, not hard-coded",
			redirect: "http://127.0.0.1:9999/spotify/cb",
			wantAddr: "127.0.0.1:9999", wantPath: "/spotify/cb",
		},
		{
			// Without this, `docker compose run --service-ports` still
			// fails: a published port forwards to eth0, not loopback.
			name:     "bind override reaches into a container",
			redirect: "http://127.0.0.1:8888/callback",
			bind:     "0.0.0.0",
			wantAddr: "0.0.0.0:8888", wantPath: "/callback",
		},
		{
			name:     "missing port falls back to the documented default",
			redirect: "http://localhost/callback",
			wantAddr: "localhost:8888", wantPath: "/callback",
		},
		{
			// Spotify calls back to "/" for a pathless redirect URI, so
			// serving "/callback" would 404 and hang until the timeout —
			// the exact failure this derivation exists to prevent.
			name:     "a pathless redirect is served at the root",
			redirect: "http://127.0.0.1:8888",
			wantAddr: "127.0.0.1:8888", wantPath: "/",
		},
		{
			name:     "an explicit root path is preserved",
			redirect: "http://127.0.0.1:8888/",
			wantAddr: "127.0.0.1:8888", wantPath: "/",
		},
		{
			name:      "a bind override carrying a port is rejected",
			redirect:  "http://127.0.0.1:8888/callback",
			bind:      "0.0.0.0:9000",
			wantError: true,
		},
		{
			name:      "an https redirect is rejected rather than bound",
			redirect:  "https://127.0.0.1:8888/callback",
			wantError: true,
		},
		{
			name:      "a redirect with no host is rejected",
			redirect:  "/callback",
			wantError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := callbackTarget(tc.redirect, tc.bind)
			if tc.wantError {
				if err == nil {
					t.Fatalf("callbackTarget(%q) = %+v, want an error", tc.redirect, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("callbackTarget(%q): %v", tc.redirect, err)
			}
			if got.Addr != tc.wantAddr {
				t.Errorf("Addr = %q, want %q", got.Addr, tc.wantAddr)
			}
			if got.Path != tc.wantPath {
				t.Errorf("Path = %q, want %q", got.Path, tc.wantPath)
			}
		})
	}
}

// clearEnv unsets every DIFMSYNC_* variable for the duration of a test.
// Without it these tests inherit the developer's real configuration —
// .env.local exports live credentials — and assertions about
// missing configuration pass or fail depending on whose machine runs
// them.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if k, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "DIFMSYNC_") {
			t.Setenv(k, "")
		}
	}
}

// runCLI drives the real command tree — flag parsing, env fallbacks and
// subcommand wiring included — against a scratch database.
func runCLI(t *testing.T, dbPath string, args ...string) error {
	t.Helper()
	full := append([]string{"difmsync", "--db-path", dbPath}, args...)
	return newApp().Run(context.Background(), full)
}

// seed builds a database with one account, one ledger row and a
// watermark, which is the state every resync scenario starts from.
func seed(t *testing.T) (string, sqlite.Account) {
	t.Helper()
	clearEnv(t)
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "cli.db")

	store, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	account, err := store.EnsureAccount(ctx, "default", "1", "PL1")
	if err != nil {
		t.Fatalf("ensure account: %v", err)
	}
	if err := store.RecordSynced(ctx, sqlite.SyncedTrack{
		AccountID: account.ID, DifmTrackID: 7, SpotifyTrackID: "sp7",
		PlaylistID: "PL1", Artist: "A", Title: "T",
		// Set deliberately: the watermark sits after this, which is the
		// situation --forget has to cope with, and a zero LikedAt would
		// make the rewind assertion below vacuous.
		LikedAt: seededLike,
	}); err != nil {
		t.Fatalf("RecordSynced: %v", err)
	}
	if err := store.SetWatermark(ctx, account.ID, seededMark); err != nil {
		t.Fatalf("SetWatermark: %v", err)
	}
	// Every scenario built on seed models a deployed, already-authorized
	// daemon, so the fixture has to carry a refresh token: an account
	// without one now reports unhealthy for that reason alone, which would
	// mask whatever the test is actually asserting about health.
	if err := store.SetSpotifyRefreshToken(ctx, account.ID, "seed-refresh-token"); err != nil {
		t.Fatalf("SetSpotifyRefreshToken: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return dbPath, account
}

var (
	seededMark = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	seededLike = time.Date(2026, 2, 14, 9, 30, 0, 0, time.UTC)
)

// inspect reopens the database to assert on persisted state.
func inspect(t *testing.T, dbPath string) (sqlite.Account, int64) {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = store.Close() }()

	account, err := store.GetAccount(ctx, "default")
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	n, err := store.CountSynced(ctx, account.ID)
	if err != nil {
		t.Fatalf("CountSynced: %v", err)
	}
	return account, n
}

// CLAUDE.md is explicit that resync must clear *both* suppressors, and
// that clearing only the ledger is a silent no-op because the watermark
// filters at fetch time.
func TestResyncClearsBothSuppressors(t *testing.T) {
	tests := []struct {
		name          string
		args          []string
		wantLedger    int64
		wantMarkClear bool
		// wantMark, when set, asserts the exact resulting watermark.
		// "not zero" is too weak for the rewind case: a watermark left
		// completely untouched satisfies it just as well as a correct
		// rewind does, which is how the no-op shipped in the first place.
		wantMark time.Time
	}{
		{
			name:       "--forget-all clears ledger and watermark",
			args:       []string{"resync", "--forget-all"},
			wantLedger: 0, wantMarkClear: true,
		},
		{
			name:       "--all clears the watermark, ledger untouched",
			args:       []string{"resync", "--all"},
			wantLedger: 1, wantMarkClear: true,
		},
		{
			// CLAUDE.md: resync must clear *both* suppressors. Dropping
			// the ledger row alone is a silent no-op, because the
			// watermark filters at fetch time and has long since moved
			// past the like. Rewound rather than cleared: --all resets
			// all of history, which is not what was asked for.
			name:       "--forget alone rewinds the watermark past the like",
			args:       []string{"resync", "--forget=7"},
			wantLedger: 0, wantMarkClear: false,
			wantMark: seededLike.Add(-time.Second),
		},
		{
			name:       "--forget with --all clears both",
			args:       []string{"resync", "--forget=7", "--all"},
			wantLedger: 0, wantMarkClear: true,
		},
		{
			// Regression: an unmatched --forget returned early and
			// silently dropped the explicit --all alongside it.
			name:       "a missed --forget must not swallow an explicit --all",
			args:       []string{"resync", "--forget=999999", "--all"},
			wantLedger: 1, wantMarkClear: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dbPath, _ := seed(t)
			if err := runCLI(t, dbPath, tc.args...); err != nil {
				t.Fatalf("resync: %v", err)
			}

			account, ledger := inspect(t, dbPath)
			if ledger != tc.wantLedger {
				t.Errorf("ledger rows = %d, want %d", ledger, tc.wantLedger)
			}
			if got := account.WatermarkLikedAt.IsZero(); got != tc.wantMarkClear {
				t.Errorf("watermark cleared = %v, want %v (watermark=%s)",
					got, tc.wantMarkClear, account.WatermarkLikedAt)
			}
			if !tc.wantMark.IsZero() && !account.WatermarkLikedAt.Equal(tc.wantMark) {
				t.Errorf("watermark = %s, want %s — the forgotten like must be re-readable",
					account.WatermarkLikedAt, tc.wantMark)
			}
		})
	}
}

// A --forget that matches nothing, with no other instruction, must fail
// rather than report success having changed nothing.
func TestResyncReportsATotalMiss(t *testing.T) {
	dbPath, _ := seed(t)
	err := runCLI(t, dbPath, "resync", "--forget=999999")
	if err == nil {
		t.Fatal("expected an error when no --forget id matched")
	}
	if !strings.Contains(err.Error(), "none of the") {
		t.Errorf("err = %v, want it to name the miss", err)
	}
}

func TestResyncWithNoFlagsIsAnError(t *testing.T) {
	dbPath, _ := seed(t)
	if err := runCLI(t, dbPath, "resync"); err == nil {
		t.Error("expected `resync` with no flags to explain itself")
	}
}

// Approving a track that was never queued must fail rather than exit
// zero having done nothing.
func TestReviewApproveUnknownTrackFails(t *testing.T) {
	dbPath, _ := seed(t)
	err := runCLI(t, dbPath, "review", "--approve=999999",
		"--spotify-client-id=id", "--spotify-client-secret=secret")
	if err == nil {
		t.Fatal("expected an error approving a track that is not queued")
	}
}

// Approval writes to Spotify, so it must say so rather than failing
// later with an opaque credentials error.
func TestReviewApproveRequiresCredentials(t *testing.T) {
	dbPath, _ := seed(t)
	err := runCLI(t, dbPath, "review", "--approve=7")
	if err == nil {
		t.Fatal("expected an error without Spotify credentials")
	}
	if !strings.Contains(err.Error(), "credentials") {
		t.Errorf("err = %v, want it to name the missing credentials", err)
	}
}

// Every required flag must be reported at once; discovering them one
// run at a time is a miserable first-time experience.
func TestSyncNamesEveryMissingFlag(t *testing.T) {
	clearEnv(t)
	dbPath := filepath.Join(t.TempDir(), "cli.db")
	err := runCLI(t, dbPath, "sync")
	if err == nil {
		t.Fatal("expected `sync` to refuse to run unconfigured")
	}
	for _, want := range []string{
		"--api-key", "--member-id", "--playlist-id",
		"--spotify-client-id", "--spotify-client-secret",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

// The database holds the Spotify refresh token, which grants unattended
// playlist write access until revoked.
func TestOpenStoreRestrictsPermissions(t *testing.T) {
	dbPath, _ := seed(t)
	if err := runCLI(t, dbPath, "status"); err != nil {
		t.Fatalf("status: %v", err)
	}

	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("db mode = %o, want 600", perm)
	}
}

// TestCallbackTargetServesTheDerivedPath closes the gap that let the
// pathless-redirect bug ship: TestCallbackTarget checks the derivation,
// but nothing stood the mux up on the derived path and issued a request.
// The bug was never in the arithmetic — it was in what the server would
// actually answer, which only a request can show.
func TestCallbackTargetServesTheDerivedPath(t *testing.T) {
	for _, redirect := range []string{
		"http://127.0.0.1:8888/callback",
		"http://127.0.0.1:8888",
		"http://127.0.0.1:8888/",
		"http://127.0.0.1:8888/oauth/spotify",
	} {
		t.Run(redirect, func(t *testing.T) {
			target, err := callbackTarget(redirect, "")
			if err != nil {
				t.Fatalf("callbackTarget(%q): %v", redirect, err)
			}

			mux := http.NewServeMux()
			mux.HandleFunc(target.Path, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			// Request the path Spotify will actually call back to — the
			// redirect URL's own path, not the one we derived.
			u, err := url.Parse(redirect)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			path := u.Path
			if path == "" {
				path = "/"
			}
			resp, err := srv.Client().Get(srv.URL + path + "?code=abc&state=xyz")
			if err != nil {
				t.Fatalf("callback request: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusOK {
				t.Errorf("GET %s = %d, want 200 — Spotify's callback would 404 and hang",
					path, resp.StatusCode)
			}
		})
	}
}

// serveWhile is what keeps the sync loop observable, so the properties
// that matter are: the listener is up before work starts, a bad address
// fails loudly rather than leaving a silent daemon, and canceling the
// context stops both halves.
func TestServeWhileServesUntilWorkStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ping", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "pong")
	})

	// Port 0 lets the OS pick, but then the test needs the real address.
	// Serving on a listener whose address we recover through a handler
	// hit would be circular, so bind explicitly and read it back.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("probe close: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- serveWhile(ctx, addr, mux, discardLogger(), func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return nil
		})
	}()

	<-started
	resp, err := http.Get("http://" + addr + "/ping")
	if err != nil {
		t.Fatalf("GET /ping: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	// Canceling is the SIGTERM path: the work returns, and serveWhile
	// returns with it rather than hanging on the still-listening server.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serveWhile returned %v, want nil on a clean stop", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serveWhile did not return after the context was canceled")
	}
}

// A daemon that syncs fine but answers nothing reads as dead to whatever
// polls it, so an unusable --http-addr must fail at startup rather than
// being logged and shrugged off.
func TestServeWhileRejectsABadAddress(t *testing.T) {
	ranWork := false
	err := serveWhile(context.Background(), "256.256.256.256:99999", http.NewServeMux(),
		discardLogger(), func(context.Context) error {
			ranWork = true
			return nil
		})
	if err == nil {
		t.Fatal("expected an error for an unbindable address")
	}
	if !strings.Contains(err.Error(), "status endpoints") {
		t.Errorf("err = %v, want it to name what failed to bind", err)
	}
	if ranWork {
		t.Error("work started despite the listener failing")
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// The backup is often the only copy of the Spotify refresh token, and
// restoring one means writing it over the live database. Every property
// here is about not producing something that looks restorable and isn't.
func TestBackupRoundTrip(t *testing.T) {
	dbPath, _ := seed(t)
	dest := filepath.Join(t.TempDir(), "nested", "backup.db")

	if err := runCLI(t, dbPath, "backup", "--to", dest); err != nil {
		t.Fatalf("backup: %v", err)
	}

	// It must open, and carry the ledger the source had.
	account, n := inspect(t, dest)
	if account.Label != "default" {
		t.Errorf("account label = %q, want default", account.Label)
	}
	if n != 1 {
		t.Errorf("synced count = %d, want 1", n)
	}

	// It holds the refresh token, so it must not be group- or
	// world-readable.
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("backup mode = %o, want 600", perm)
	}
}

// SQLite refuses to VACUUM INTO an existing path. That refusal is kept
// rather than papered over with a truncate, because the file it would
// overwrite is frequently the only copy of a refresh token.
func TestBackupRefusesAnExistingDestination(t *testing.T) {
	dbPath, _ := seed(t)
	dest := filepath.Join(t.TempDir(), "backup.db")

	if err := runCLI(t, dbPath, "backup", "--to", dest); err != nil {
		t.Fatalf("first backup: %v", err)
	}
	before, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if err := runCLI(t, dbPath, "backup", "--to", dest); err == nil {
		t.Fatal("expected the second backup to refuse an existing destination")
	}

	after, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("reread: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("the refused backup modified the existing file")
	}
}

// A wrong --db-path yields a database with no account row. Reporting that
// as a successful backup is the failure mode the verification exists to
// prevent, and leaving the file behind is how it gets restored later by
// someone who never saw the error.
func TestBackupRejectsAndRemovesAnAccountlessSnapshot(t *testing.T) {
	clearEnv(t)
	empty := filepath.Join(t.TempDir(), "empty.db")
	dest := filepath.Join(t.TempDir(), "backup.db")

	err := runCLI(t, empty, "backup", "--to", dest)
	if err == nil {
		t.Fatal("expected a backup of an accountless database to fail")
	}
	if !strings.Contains(err.Error(), "account row") {
		t.Errorf("err = %v, want it to name the missing account row", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Errorf("the unusable backup was left at %s", dest)
	}
}

// A failed backup must leave nothing at the destination.
//
// VACUUM INTO does not clean up its own partial output, and the nightly
// cron writes into the same volume as the database — so a full volume
// used to leave a truncated file with a plausible dated name, which is
// exactly what a later restore would copy over the live database. The
// staging directory plus rename means only a verified snapshot ever
// appears at dest.
func TestBackupLeavesNothingBehindOnFailure(t *testing.T) {
	clearEnv(t)
	// A database with no account row fails verification, which stands in
	// for any mid-backup failure: the question is what is left at dest.
	empty := filepath.Join(t.TempDir(), "empty.db")
	dir := t.TempDir()
	dest := filepath.Join(dir, "backup.db")

	if err := runCLI(t, empty, "backup", "--to", dest); err == nil {
		t.Fatal("expected the backup to fail")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("destination %s exists after a failed backup", dest)
	}

	// And no staging directory left lying around either.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		t.Errorf("leftover in the destination directory: %s", e.Name())
	}
}

// The snapshot holds the Spotify refresh token, so it must never be
// world-readable — not even briefly. VACUUM INTO creates its output with
// the process umask (0644 by default) and it can only be chmod'd once the
// copy finishes, so the file is staged inside a 0700 directory instead.
func TestBackupIsNeverWorldReadable(t *testing.T) {
	dbPath, _ := seed(t)
	dir := t.TempDir()
	dest := filepath.Join(dir, "backup.db")

	if err := runCLI(t, dbPath, "backup", "--to", dest); err != nil {
		t.Fatalf("backup: %v", err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("backup mode = %o, want 600", perm)
	}
	// The staging directory is what closes the window during the copy;
	// assert it is cleaned up rather than left behind at 0700.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "backup.db" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("destination directory holds %v, want just backup.db", names)
	}
}

// TestStatusCheckIsTheHealthcheckContract exercises `status --check`
// through the CLI, with the exact argv compose.yaml's healthcheck runs.
//
// internal/status covers the health rule itself; what nothing covered was
// the wiring between that rule and the command line. compose.yaml pins
// `["CMD", "/app/difmsync", "status", "--check"]` and reads *only* the
// exit code, so a renamed flag, a subcommand that stops returning an
// error, or --max-age losing its env source all ship green here and
// surface as a container that is permanently unhealthy — a failure mode
// with no error message anywhere, because the check never ran.
func TestStatusCheckIsTheHealthcheckContract(t *testing.T) {
	ctx := context.Background()

	// recordRun writes one finished, non-dry run backdated by age.
	recordRun := func(t *testing.T, dbPath string, accountID int64, age time.Duration) {
		t.Helper()
		store, err := sqlite.Open(dbPath)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { _ = store.Close() }()
		at := time.Now().Add(-age)
		store.SetClock(func() time.Time { return at })
		id, err := store.StartRun(ctx, accountID, false)
		if err != nil {
			t.Fatalf("StartRun: %v", err)
		}
		if err := store.FinishRun(ctx, id, sqlite.RunStats{Added: 1}); err != nil {
			t.Fatalf("FinishRun: %v", err)
		}
	}

	t.Run("fresh clean pass exits zero", func(t *testing.T) {
		dbPath, account := seed(t)
		recordRun(t, dbPath, account.ID, time.Minute)
		if err := runCLI(t, dbPath, "status", "--check"); err != nil {
			t.Errorf("status --check on a fresh pass = %v, want nil", err)
		}
	})

	t.Run("stale pass exits non-zero", func(t *testing.T) {
		dbPath, account := seed(t)
		recordRun(t, dbPath, account.ID, 3*time.Hour)
		err := runCLI(t, dbPath, "status", "--check")
		if err == nil {
			t.Fatal("status --check on a 3h-old pass = nil, want an error")
		}
		// The message is the healthcheck's only output, and it lands in
		// `docker inspect`. An empty or generic one is why an operator
		// ends up reading source to find out what is wrong.
		if !strings.Contains(err.Error(), "last clean pass") {
			t.Errorf("unhealthy reason = %q, want it to name the stale pass", err)
		}
	})

	t.Run("before auth exits non-zero", func(t *testing.T) {
		// The pre-auth window is why compose.yaml sets a 30m start_period.
		// A fresh deployment has no account row at all, and the check has
		// to fail rather than panic on the missing row.
		clearEnv(t)
		dbPath := filepath.Join(t.TempDir(), "empty.db")
		if err := runCLI(t, dbPath, "status", "--check"); err == nil {
			t.Error("status --check with no account = nil, want an error")
		}
	})

	t.Run("--max-age reads DIFMSYNC_STATUS_MAX_AGE", func(t *testing.T) {
		// docs/deploy.md tells operators to shrink --max-age to see the
		// unhealthy branch without waiting out a real stall, and the
		// compose healthcheck has no way to pass a flag — it goes through
		// the environment or not at all.
		dbPath, account := seed(t)
		recordRun(t, dbPath, account.ID, 10*time.Minute)
		if err := runCLI(t, dbPath, "status", "--check"); err != nil {
			t.Fatalf("precondition: 10m-old pass should be healthy at the default max-age: %v", err)
		}
		t.Setenv("DIFMSYNC_STATUS_MAX_AGE", "1s")
		if err := runCLI(t, dbPath, "status", "--check"); err == nil {
			t.Error("DIFMSYNC_STATUS_MAX_AGE=1s did not make a 10m-old pass unhealthy")
		}
	})
}
