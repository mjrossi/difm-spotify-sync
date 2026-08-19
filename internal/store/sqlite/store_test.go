package sqlite_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

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
