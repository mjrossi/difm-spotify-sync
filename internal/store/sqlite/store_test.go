package sqlite_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/pressly/goose/v3"

	migrations "github.com/mjrossi/difm-spotify-sync/migrations-sqlite"

	"github.com/mjrossi/difm-spotify-sync/internal/store/sqlite"
	"github.com/mjrossi/difm-spotify-sync/pkg/match"
)

func newTestStore(t *testing.T) *sqlite.Store {
	t.Helper()
	// A file in t.TempDir() rather than :memory: — the connection pool is
	// capped at 1, so this also exercises the real WAL/pragma path.
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return s
}

func TestMigrateIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	// Migrate runs on every boot; a second call must be a no-op.
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}

func TestSyncedTrackLedgerIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	acct, err := s.EnsureAccount(ctx, "default", "10000001", "playlist123")
	if err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}

	entry := sqlite.SyncedTrack{
		AccountID:      acct.ID,
		DifmTrackID:    3041427,
		DifmVoteID:     64755876,
		SpotifyTrackID: "spotify123",
		PlaylistID:     "playlist123",
		Artist:         "Funk D'Void & Berny",
		Title:          "Junkies (Joe Silva Remix)",
		MatchScore:     0.94,
		LikedAt:        time.Date(2026, 2, 28, 16, 2, 17, 0, time.UTC),
	}

	// Recording the same like twice must not duplicate it — this is the
	// guarantee that makes a second sync pass add nothing.
	for i := range 2 {
		if err := s.RecordSynced(ctx, entry); err != nil {
			t.Fatalf("RecordSynced #%d: %v", i+1, err)
		}
	}

	n, err := s.CountSynced(ctx, acct.ID)
	if err != nil {
		t.Fatalf("CountSynced: %v", err)
	}
	if n != 1 {
		t.Errorf("CountSynced = %d, want 1 after a duplicate write", n)
	}

	synced, err := s.IsSynced(ctx, acct.ID, 3041427, "playlist123")
	if err != nil {
		t.Fatalf("IsSynced: %v", err)
	}
	if !synced {
		t.Error("IsSynced = false, want true")
	}

	// A different playlist is a different destination, not a duplicate.
	other, err := s.IsSynced(ctx, acct.ID, 3041427, "another-playlist")
	if err != nil {
		t.Fatalf("IsSynced(other): %v", err)
	}
	if other {
		t.Error("IsSynced = true for a playlist the track was never added to")
	}
}

func TestEnsureAccountUpsertsAndPreservesToken(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	acct, err := s.EnsureAccount(ctx, "default", "111", "playlistA")
	if err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}
	if err := s.SetSpotifyRefreshToken(ctx, acct.ID, "refresh-token"); err != nil {
		t.Fatalf("SetSpotifyRefreshToken: %v", err)
	}

	// Re-running sync with a changed playlist must not discard the
	// refresh token, or every config tweak would force re-auth.
	again, err := s.EnsureAccount(ctx, "default", "111", "playlistB")
	if err != nil {
		t.Fatalf("EnsureAccount again: %v", err)
	}
	if again.ID != acct.ID {
		t.Errorf("account id changed: %d -> %d", acct.ID, again.ID)
	}
	if again.SpotifyPlaylistID != "playlistB" {
		t.Errorf("playlist = %q, want playlistB", again.SpotifyPlaylistID)
	}
	if again.SpotifyRefreshToken != "refresh-token" {
		t.Errorf("refresh token = %q, want it preserved", again.SpotifyRefreshToken)
	}
}

func TestWatermarkRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	acct, err := s.EnsureAccount(ctx, "default", "111", "p")
	if err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}
	if !acct.WatermarkLikedAt.IsZero() {
		t.Errorf("fresh account watermark = %s, want zero", acct.WatermarkLikedAt)
	}

	want := time.Date(2026, 2, 28, 16, 2, 17, 0, time.UTC)
	if err := s.SetWatermark(ctx, acct.ID, want); err != nil {
		t.Fatalf("SetWatermark: %v", err)
	}
	got, err := s.GetAccount(ctx, "default")
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	if !got.WatermarkLikedAt.Equal(want) {
		t.Errorf("watermark = %s, want %s", got.WatermarkLikedAt, want)
	}
}

func TestReviewQueueLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	acct, err := s.EnsureAccount(ctx, "default", "111", "p")
	if err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}

	item := sqlite.ReviewItem{
		AccountID:   acct.ID,
		DifmTrackID: 42,
		Artist:      "DJ Rax",
		Title:       "Air Race (Spiritchaser Remix)",
		DurationSec: 480,
		Reason:      sqlite.ReasonLowConfidence,
		BestScore:   0.72,
		LikedAt:     time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC),
		Candidates: []match.Scored{
			{Candidate: match.Candidate{ID: "abc", Artist: "DJ Rax", Title: "Air Race"}, Score: 0.72, Why: "version conflict"},
		},
	}
	if err := s.Enqueue(ctx, item); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	pending, err := s.ListReview(ctx, acct.ID, "pending", 10)
	if err != nil {
		t.Fatalf("ListReview: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want 1", len(pending))
	}
	// Candidates must survive the JSON round-trip — they're the whole
	// point of the queue for a human deciding a borderline call.
	if len(pending[0].Candidates) != 1 || pending[0].Candidates[0].ID != "abc" {
		t.Errorf("candidates lost in round-trip: %+v", pending[0].Candidates)
	}
	if pending[0].Reason != sqlite.ReasonLowConfidence {
		t.Errorf("reason = %q", pending[0].Reason)
	}

	// Re-queuing the same track updates it rather than duplicating —
	// and refreshes *every* column carrying fresh data, not just the
	// score. The upsert originally touched three, leaving artist, title,
	// liked_at and details_url showing whatever the first queue saw,
	// which is what a human then reviews.
	item.BestScore = 0.80
	item.Artist = "Corrected Artist"
	item.Title = "Corrected Title"
	item.DetailsURL = "https://www.di.fm/tracks/42/corrected"
	item.LikedAt = item.LikedAt.Add(48 * time.Hour)
	if err := s.Enqueue(ctx, item); err != nil {
		t.Fatalf("Enqueue again: %v", err)
	}
	pending, err = s.ListReview(ctx, acct.ID, "pending", 10)
	if err != nil {
		t.Fatalf("ListReview again: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending = %d after re-queue, want 1", len(pending))
	}
	got := pending[0]
	if got.Artist != "Corrected Artist" || got.Title != "Corrected Title" {
		t.Errorf("artist/title = %q/%q, want the re-queued values", got.Artist, got.Title)
	}
	if got.DetailsURL != "https://www.di.fm/tracks/42/corrected" {
		t.Errorf("DetailsURL = %q, want the re-queued value", got.DetailsURL)
	}
	if !got.LikedAt.Equal(item.LikedAt) {
		t.Errorf("LikedAt = %s, want %s", got.LikedAt, item.LikedAt)
	}
	if got.BestScore != 0.80 {
		t.Errorf("BestScore = %v, want 0.80", got.BestScore)
	}

	ok, err := s.ResolveReview(ctx, acct.ID, 42, "approved")
	if err != nil {
		t.Fatalf("ResolveReview: %v", err)
	}
	if !ok {
		t.Error("ResolveReview reported no row matched")
	}
	pending, err = s.ListReview(ctx, acct.ID, "pending", 10)
	if err != nil {
		t.Fatalf("ListReview after resolve: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("pending = %d after approval, want 0", len(pending))
	}
}

func TestSyncRunRecordsFailures(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	acct, err := s.EnsureAccount(ctx, "default", "111", "p")
	if err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}
	runID, err := s.StartRun(ctx, acct.ID, false)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	// A failed pass must still be closed out — an unrecorded failure is
	// how a broken sync goes unnoticed on a headless box.
	if err := s.FinishRun(ctx, runID, sqlite.RunStats{
		Fetched: 3, Added: 1, Queued: 2, Err: context.DeadlineExceeded,
	}); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
}

// TestResyncRecoveryPath covers the escape hatch. Both suppressors have to
// be cleared: the ledger row AND the watermark. Clearing only the ledger
// leaves the like unreachable, because the watermark filters at fetch time.
func TestResyncRecoveryPath(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	acct, err := s.EnsureAccount(ctx, "default", "111", "p")
	if err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}
	entry := sqlite.SyncedTrack{
		AccountID: acct.ID, DifmTrackID: 42, SpotifyTrackID: "sp1", PlaylistID: "p",
		LikedAt: time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC),
	}
	if err := s.RecordSynced(ctx, entry); err != nil {
		t.Fatalf("RecordSynced: %v", err)
	}
	if err := s.SetWatermark(ctx, acct.ID, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("SetWatermark: %v", err)
	}

	found, err := s.ForgetTrack(ctx, acct.ID, 42)
	if err != nil {
		t.Fatalf("ForgetTrack: %v", err)
	}
	if !found {
		t.Error("ForgetTrack reported no match for a row that exists")
	}

	// A mistyped id must report itself rather than silently no-op.
	missing, err := s.ForgetTrack(ctx, acct.ID, 999999)
	if err != nil {
		t.Fatalf("ForgetTrack(missing): %v", err)
	}
	if missing {
		t.Error("ForgetTrack reported a match for a track that was never synced")
	}
	synced, err := s.IsSynced(ctx, acct.ID, 42, "p")
	if err != nil {
		t.Fatalf("IsSynced: %v", err)
	}
	if synced {
		t.Error("track still marked synced after ForgetTrack")
	}

	if err := s.ClearWatermark(ctx, acct.ID); err != nil {
		t.Fatalf("ClearWatermark: %v", err)
	}
	got, err := s.GetAccount(ctx, "default")
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	if !got.WatermarkLikedAt.IsZero() {
		t.Errorf("watermark = %s, want zero so history is re-read", got.WatermarkLikedAt)
	}
}

func TestForgetAllTracks(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	acct, err := s.EnsureAccount(ctx, "default", "111", "p")
	if err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}
	for id := int64(1); id <= 3; id++ {
		if err := s.RecordSynced(ctx, sqlite.SyncedTrack{
			AccountID: acct.ID, DifmTrackID: id, SpotifyTrackID: "sp", PlaylistID: "p",
		}); err != nil {
			t.Fatalf("RecordSynced: %v", err)
		}
	}
	if err := s.ForgetAllTracks(ctx, acct.ID); err != nil {
		t.Fatalf("ForgetAllTracks: %v", err)
	}
	n, err := s.CountSynced(ctx, acct.ID)
	if err != nil {
		t.Fatalf("CountSynced: %v", err)
	}
	if n != 0 {
		t.Errorf("CountSynced = %d, want 0", n)
	}
}

// A mistyped id must not exit successfully having changed nothing —
// the same reason ForgetTrack reports whether it matched.
func TestResolveReviewReportsAMiss(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	acct, err := s.EnsureAccount(ctx, "default", "1", "PL1")
	if err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}

	ok, err := s.ResolveReview(ctx, acct.ID, 999999, "approved")
	if err != nil {
		t.Fatalf("ResolveReview: %v", err)
	}
	if ok {
		t.Error("ResolveReview claimed to resolve a track that was never queued")
	}
}

// CountReview must count the table, not a capped listing.
func TestCountReviewIsNotCappedByAListingLimit(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	acct, err := s.EnsureAccount(ctx, "default", "1", "PL1")
	if err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}
	for i := range 25 {
		if err := s.Enqueue(ctx, sqlite.ReviewItem{
			AccountID:   acct.ID,
			DifmTrackID: int64(i + 1),
			Artist:      "A",
			Title:       "T",
			Reason:      sqlite.ReasonNoMatch,
		}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	n, err := s.CountReview(ctx, acct.ID, "pending")
	if err != nil {
		t.Fatalf("CountReview: %v", err)
	}
	if n != 25 {
		t.Errorf("CountReview = %d, want 25", n)
	}
}

// InTx must roll back every write when fn fails, so a ledger row cannot
// land without the watermark that accompanies it (or vice versa).
func TestInTxRollsBackOnError(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	acct, err := s.EnsureAccount(ctx, "default", "1", "PL1")
	if err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}

	sentinel := errors.New("boom")
	err = s.InTx(ctx, func(tx *sqlite.Store) error {
		if err := tx.RecordSynced(ctx, sqlite.SyncedTrack{
			AccountID: acct.ID, DifmTrackID: 1, SpotifyTrackID: "sp1",
			PlaylistID: "PL1", Artist: "A", Title: "T",
		}); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("InTx err = %v, want the sentinel", err)
	}

	n, err := s.CountSynced(ctx, acct.ID)
	if err != nil {
		t.Fatalf("CountSynced: %v", err)
	}
	if n != 0 {
		t.Errorf("ledger has %d row(s) after a rolled-back transaction, want 0", n)
	}
}

// TestDSNPreservesAnExistingQueryString: the PRAGMAs are appended to
// whatever the caller passed, so a path that already carries a query
// string must gain "&_pragma=..." rather than a second "?", which
// produces a DSN the driver cannot parse.
func TestDSNPreservesAnExistingQueryString(t *testing.T) {
	dir := t.TempDir()
	// _txlock is a real modernc driver parameter, so this is a DSN a
	// caller could plausibly build.
	path := filepath.Join(dir, "q.db") + "?_txlock=immediate"

	s, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("Open with an existing query string: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if _, err := s.EnsureAccount(context.Background(), "default", "1", "PL1"); err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}
}

// TestNestedInTxIsRejected: SQLite has one writer, so a nested BeginTx
// blocks until the context expires — and the daemon's context is a
// signal context with no deadline, so it blocks forever. An error is the
// only outcome that does not look like an unexplained hang.
func TestNestedInTxIsRejected(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	err := s.InTx(ctx, func(tx *sqlite.Store) error {
		return tx.InTx(ctx, func(*sqlite.Store) error { return nil })
	})
	if err == nil {
		t.Fatal("nested InTx returned nil; it must refuse rather than deadlock")
	}
	if !strings.Contains(err.Error(), "already in a transaction") {
		t.Errorf("err = %v, want it to name the nesting", err)
	}
}

// TestCorruptWatermarkWarnsRatherThanSilentlyZeroing: an unparseable
// watermark fails safe — a zero value re-reads everything rather than
// skipping likes — but a full re-read on every tick with no explanation
// is a mystery worth an hour of someone's evening. The warning is the
// only signal, so it is worth a test.
func TestCorruptWatermarkWarnsRatherThanSilentlyZeroing(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "corrupt.db")
	ctx := context.Background()

	s, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	acct, err := s.EnsureAccount(ctx, "default", "1", "PL1")
	if err != nil {
		t.Fatalf("ensure account: %v", err)
	}

	// Write a value no format this store uses can parse.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx,
		`UPDATE accounts SET watermark_liked_at = 'not-a-timestamp' WHERE id = ?`, acct.ID); err != nil {
		t.Fatalf("corrupt the watermark: %v", err)
	}

	var buf bytes.Buffer
	s.SetLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))

	got, err := s.GetAccount(ctx, "default")
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	if !got.WatermarkLikedAt.IsZero() {
		t.Errorf("watermark = %s, want zero so the next pass re-reads everything", got.WatermarkLikedAt)
	}
	if !strings.Contains(buf.String(), "unparseable watermark") {
		t.Errorf("logged %q, want a warning naming the unparseable watermark", buf.String())
	}
}

// TestFinishRunRecordsTheKind: the kind is what /healthz is allowed to
// say about a failed pass, so it has to round-trip through the row.
func TestFinishRunRecordsTheKind(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	acct, err := s.EnsureAccount(ctx, "default", "111", "p")
	if err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}
	runID, err := s.StartRun(ctx, acct.ID, false)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if err := s.FinishRun(ctx, runID, sqlite.RunStats{
		Err: errors.New("difm: page 1: unauthorized"), Kind: sqlite.KindDiFMUnauthorized,
	}); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	runs, err := s.ListRuns(ctx, acct.ID, 1)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].ErrorKind != sqlite.KindDiFMUnauthorized {
		t.Fatalf("ListRuns = %+v, want one run with ErrorKind %q", runs, sqlite.KindDiFMUnauthorized)
	}
}

// TestErrorKindDefaultsForPreexistingRows: a database written by v1.0.0
// has sync_runs rows with no kind. After migrating they must read as the
// empty kind, which status treats exactly as it did before the column
// existed — not fail to scan, and not report something invented.
func TestErrorKindDefaultsForPreexistingRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")

	// Build the v1.0.0 schema by hand: migrate only to 0001, then write a
	// row through raw SQL, since the Store API of this version cannot
	// produce a row without a kind. Drives goose directly, outside
	// Store.Migrate and its mutex; that is safe only because this
	// package does not use t.Parallel.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("dialect: %v", err)
	}
	if err := goose.UpToContext(ctx, raw, ".", 1); err != nil {
		t.Fatalf("goose up-to 1: %v", err)
	}
	for _, stmt := range []string{
		`INSERT INTO accounts (id, label) VALUES (1, 'default')`,
		`INSERT INTO sync_runs (account_id, finished_at, error) VALUES (1, '2026-01-01T00:00:00.000Z', 'boom')`,
	} {
		if _, err := raw.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	s, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	runs, err := s.ListRuns(ctx, 1, 1)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("ListRuns returned %d rows, want 1", len(runs))
	}
	if runs[0].ErrorKind != "" || runs[0].Error != "boom" {
		t.Errorf("run = %+v, want ErrorKind \"\" and Error \"boom\"", runs[0])
	}
}

// TestFinishRunRejectsAnUnknownKind: RunErrorKind is a plain string, so
// nothing at compile time stops a caller from passing error text through
// Kind instead of one of the package's sentinels. The column is
// published by the status endpoints, so FinishRun must refuse it rather
// than write it verbatim — and it must say so in the log, the same way
// an unparseable watermark does, rather than fail silently.
func TestFinishRunRejectsAnUnknownKind(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	var buf bytes.Buffer
	s.SetLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))

	acct, err := s.EnsureAccount(ctx, "default", "111", "p")
	if err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}
	runID, err := s.StartRun(ctx, acct.ID, false)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	smuggled := "difm: page 1: https://api.audioaddict.com/v1/di/members/4242/track_votes"
	if err := s.FinishRun(ctx, runID, sqlite.RunStats{
		Err:  errors.New("boom"),
		Kind: sqlite.RunErrorKind(smuggled),
	}); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	runs, err := s.ListRuns(ctx, acct.ID, 1)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].ErrorKind != sqlite.KindError {
		t.Fatalf("ListRuns = %+v, want one run with ErrorKind %q", runs, sqlite.KindError)
	}
	if strings.Contains(string(runs[0].ErrorKind), "4242") {
		t.Errorf("ErrorKind = %q, want the member id scrubbed rather than written through", runs[0].ErrorKind)
	}
	if !strings.Contains(buf.String(), "unknown error kind") {
		t.Errorf("logged %q, want a warning naming the rejected kind", buf.String())
	}
}

// TestPruneRunsKeepsTheWindowAndTheInFlightRow: retention is by age, but
// the newest rows survive regardless — the health rule reads them — and
// a row that has not finished is never a candidate, however old its
// start looks to a rewound clock.
func TestPruneRunsKeepsTheWindowAndTheInFlightRow(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	acct, err := s.EnsureAccount(ctx, "default", "111", "p")
	if err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	finished := func(age time.Duration) {
		t.Helper()
		s.SetClock(func() time.Time { return base.Add(-age) })
		id, err := s.StartRun(ctx, acct.ID, false)
		if err != nil {
			t.Fatalf("StartRun: %v", err)
		}
		if err := s.FinishRun(ctx, id, sqlite.RunStats{}); err != nil {
			t.Fatalf("FinishRun: %v", err)
		}
	}
	// 30 finished rows, one per day, the oldest 30 days old.
	for d := 30; d >= 1; d-- {
		finished(time.Duration(d) * 24 * time.Hour)
	}
	// One in-flight row that looks 40 days old.
	s.SetClock(func() time.Time { return base.Add(-40 * 24 * time.Hour) })
	if _, err := s.StartRun(ctx, acct.ID, false); err != nil {
		t.Fatalf("StartRun (in-flight): %v", err)
	}
	s.SetClock(func() time.Time { return base })

	// Retain 10 days, keep at least 5: rows 11..30 days old are
	// candidates (20 rows); the floor of 5 is already satisfied by the
	// newest 10, so all 20 go. The in-flight row stays.
	n, err := s.PruneRuns(ctx, acct.ID, base.Add(-10*24*time.Hour), 5)
	if err != nil {
		t.Fatalf("PruneRuns: %v", err)
	}
	if n != 20 {
		t.Errorf("pruned %d rows, want 20", n)
	}
	runs, err := s.ListRuns(ctx, acct.ID, 100)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 11 {
		t.Fatalf("%d rows remain, want 11 (10 recent + in-flight)", len(runs))
	}
	inFlight := 0
	for _, r := range runs {
		if r.FinishedAt == "" {
			inFlight++
		}
	}
	if inFlight != 1 {
		t.Errorf("in-flight rows remaining = %d, want 1", inFlight)
	}

	// Now retain nothing by age but keep 8: the floor is what saves rows.
	n, err = s.PruneRuns(ctx, acct.ID, base.Add(time.Hour), 8)
	if err != nil {
		t.Fatalf("PruneRuns (floor): %v", err)
	}
	// 11 rows; newest 8 by started_at are kept — the in-flight row is
	// oldest by started_at and is protected by finished_at, not the floor.
	// Candidates: 11 - 8 = 3, minus the in-flight one = 2.
	if n != 2 {
		t.Errorf("pruned %d rows under the floor, want 2", n)
	}
	// Idempotent.
	n, err = s.PruneRuns(ctx, acct.ID, base.Add(time.Hour), 8)
	if err != nil || n != 0 {
		t.Errorf("second prune = (%d, %v), want (0, nil)", n, err)
	}
}

// Both account_id filters are load-bearing: the floor subquery is
// exactly where dropping the inner one would keep another account's
// newest rows and delete this one's.
func TestPruneRunsIsScopedToTheAccount(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	a, err := s.EnsureAccount(ctx, "a", "1", "p")
	if err != nil {
		t.Fatalf("EnsureAccount a: %v", err)
	}
	b, err := s.EnsureAccount(ctx, "b", "2", "p")
	if err != nil {
		t.Fatalf("EnsureAccount b: %v", err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return base.Add(-30 * 24 * time.Hour) })
	for _, id := range []int64{a.ID, b.ID, b.ID} {
		run, err := s.StartRun(ctx, id, false)
		if err != nil {
			t.Fatalf("StartRun: %v", err)
		}
		if err := s.FinishRun(ctx, run, sqlite.RunStats{}); err != nil {
			t.Fatalf("FinishRun: %v", err)
		}
	}
	s.SetClock(func() time.Time { return base })

	if n, err := s.PruneRuns(ctx, a.ID, base, 0); err != nil || n != 1 {
		t.Fatalf("prune a = (%d, %v), want (1, nil)", n, err)
	}
	runs, err := s.ListRuns(ctx, b.ID, 10)
	if err != nil {
		t.Fatalf("ListRuns b: %v", err)
	}
	if len(runs) != 2 {
		t.Errorf("account b has %d rows after pruning a, want 2", len(runs))
	}
}

// TestOpenRefusesACorruptDatabase: the documented restore is a docker cp,
// and a truncated or half-written copy used to surface as whatever query
// tripped first — from inside a pass, under a restart policy, with no
// file name and no next step. Now the open itself says which file and
// where the runbook is.
func TestOpenRefusesACorruptDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.db")
	s := openAt(t, path)

	// A freshly-migrated, insert-only database has nothing to free, so
	// there is no freelist page for the corrupting write below to land
	// on harmlessly — every page in size/2's neighborhood is live. Asked
	// over a second raw connection to the same file: Store exposes no
	// pragma escape hatch, and WAL allows the concurrent reader.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	var freelist int
	if err := raw.QueryRowContext(context.Background(), "PRAGMA freelist_count").Scan(&freelist); err != nil {
		t.Fatalf("freelist_count: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("raw close: %v", err)
	}
	if freelist != 0 {
		t.Fatalf("freelist_count = %d, want 0 (corrupting write may land on a free page)", freelist)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Overwrite the middle of the file. Page 1 stays intact so SQLite
	// still recognizes the header and reaches the check; a page inside
	// the btree does not.
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open file: %v", err)
	}
	info, err := f.Stat()
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	garbage := bytes.Repeat([]byte{0xFF}, 512)
	if _, err := f.WriteAt(garbage, info.Size()/2); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close file: %v", err)
	}

	_, err = sqlite.Open(path)
	if err == nil {
		t.Fatal("Open succeeded on a corrupt database")
	}
	if !errors.Is(err, sqlite.ErrCorrupt) {
		t.Errorf("Open error = %q, want errors.Is ErrCorrupt", err)
	}
	for _, want := range []string{path, "Restoring"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Open error = %q, want it to contain %q", err, want)
		}
	}
}

// TestOpenRefusesATruncatedDatabase covers the case the check exists
// for: an interrupted docker cp. SQLite validates the page header when
// the connection opens, before quick_check ever runs, so a truncated
// file fails at the ping — with the old message, that read
// "sqlite.Open: ping: database disk image is malformed (11)" and gave
// no path and no next step.
func TestOpenRefusesATruncatedDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "truncated.db")
	s := openAt(t, path)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if err := os.Truncate(path, info.Size()/2); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	_, err = sqlite.Open(path)
	if err == nil {
		t.Fatal("Open succeeded on a truncated database")
	}
	if !errors.Is(err, sqlite.ErrCorrupt) {
		t.Errorf("Open error = %q, want errors.Is ErrCorrupt", err)
	}
	for _, want := range []string{path, "Restoring"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Open error = %q, want it to contain %q", err, want)
		}
	}
}

// TestOpenRefusesANonDatabaseFile covers a restore landing the wrong
// file entirely — the header check trips exactly as it does for a
// truncated one.
func TestOpenRefusesANonDatabaseFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-database.db")
	if err := os.WriteFile(path, []byte("this is not a database"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := sqlite.Open(path)
	if err == nil {
		t.Fatal("Open succeeded on a non-database file")
	}
	if !errors.Is(err, sqlite.ErrCorrupt) {
		t.Errorf("Open error = %q, want errors.Is ErrCorrupt", err)
	}
	for _, want := range []string{path, "Restoring"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Open error = %q, want it to contain %q", err, want)
		}
	}
}

// TestSnapshotToVerifiesBeforePublishing: the snapshot is what a restore
// copies over the live database, so an unusable one must never reach the
// destination to be mistaken for a good one later.
func TestSnapshotToVerifiesBeforePublishing(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.EnsureAccount(ctx, "default", "111", "p"); err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}
	dir := t.TempDir()

	dest := filepath.Join(dir, "good.db")
	if err := s.SnapshotTo(ctx, dest, "default"); err != nil {
		t.Fatalf("SnapshotTo: %v", err)
	}
	snap, err := sqlite.Open(dest)
	if err != nil {
		t.Fatalf("the snapshot does not open: %v", err)
	}
	defer func() { _ = snap.Close() }()
	if _, err := snap.GetAccount(ctx, "default"); err != nil {
		t.Errorf("the snapshot has no account row: %v", err)
	}

	// A verify that cannot pass must leave nothing behind at all — not a
	// partial file with a plausible name.
	missing := filepath.Join(dir, "bad.db")
	if err := s.SnapshotTo(ctx, missing, "no-such-account"); err == nil {
		t.Fatal("SnapshotTo with an unknown account returned nil")
	}
	if _, err := os.Stat(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a failed verify left %s behind (stat err = %v)", missing, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".difmsync-backup-") {
			t.Errorf("staging directory %s left behind", e.Name())
		}
	}
}

func TestSnapshotToRefusesAnExistingDestination(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.EnsureAccount(ctx, "default", "111", "p"); err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "taken.db")
	if err := os.WriteFile(dest, []byte("existing"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	err := s.SnapshotTo(ctx, dest, "default")
	if err == nil {
		t.Fatal("SnapshotTo overwrote an existing file")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("err = %v, want it to say the destination already exists", err)
	}
	// And the file it refused to overwrite is untouched.
	b, err := os.ReadFile(dest)
	if err != nil || string(b) != "existing" {
		t.Errorf("the existing file was modified: %q, %v", b, err)
	}
}

// openAt opens and migrates a store at a known path, then seeds enough
// rows that the file spans several pages.
func openAt(t *testing.T, path string) *sqlite.Store {
	t.Helper()
	ctx := context.Background()
	s, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	acct, err := s.EnsureAccount(ctx, "default", "111", "p")
	if err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}
	for range 200 {
		id, err := s.StartRun(ctx, acct.ID, false)
		if err != nil {
			t.Fatalf("StartRun: %v", err)
		}
		if err := s.FinishRun(ctx, id, sqlite.RunStats{Err: errors.New(strings.Repeat("x", 200))}); err != nil {
			t.Fatalf("FinishRun: %v", err)
		}
	}
	return s
}
