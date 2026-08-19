package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	sqlitegen "github.com/mjrossi/difm-spotify-sync/internal/store/sqlite/gen"
	"github.com/mjrossi/difm-spotify-sync/pkg/match"
)

// Review reasons. A like that does not auto-add always lands in the
// review queue under one of these, so nothing is silently dropped.
const (
	ReasonLowConfidence = "low_confidence"
	ReasonNoMatch       = "no_match"
	ReasonSkipped       = "skipped"
)

// Account is the resolved per-user configuration and sync state.
type Account struct {
	ID                  int64
	Label               string
	DifmMemberID        string
	SpotifyPlaylistID   string
	SpotifyRefreshToken string
	WatermarkLikedAt    time.Time
}

// EnsureAccount creates or updates the account row and returns it.
func (s *Store) EnsureAccount(ctx context.Context, label, memberID, playlistID string) (Account, error) {
	row, err := s.q.UpsertAccount(ctx, sqlitegen.UpsertAccountParams{
		Label:             label,
		DifmMemberID:      memberID,
		SpotifyPlaylistID: playlistID,
	})
	if err != nil {
		return Account{}, fmt.Errorf("sqlite.EnsureAccount: %w", err)
	}
	return s.toAccount(row), nil
}

// GetAccount looks up an account by label.
func (s *Store) GetAccount(ctx context.Context, label string) (Account, error) {
	row, err := s.q.GetAccountByLabel(ctx, label)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, fmt.Errorf("sqlite.GetAccount(%q): %w", label, err)
	}
	if err != nil {
		return Account{}, fmt.Errorf("sqlite.GetAccount: %w", err)
	}
	return s.toAccount(row), nil
}

func (s *Store) toAccount(row sqlitegen.Account) Account {
	a := Account{
		ID:                  row.ID,
		Label:               row.Label,
		DifmMemberID:        row.DifmMemberID,
		SpotifyPlaylistID:   row.SpotifyPlaylistID,
		SpotifyRefreshToken: row.SpotifyRefreshToken,
	}
	if row.WatermarkLikedAt != "" {
		ts, err := time.Parse(TimeFormat, row.WatermarkLikedAt)
		if err != nil {
			// Fail safe — a zero watermark re-reads everything rather
			// than skipping — but not silently: a full re-read on every
			// tick is otherwise a mystery.
			s.log.Warn("unparseable watermark; treating as unset",
				"account", row.Label, "value", row.WatermarkLikedAt, "err", err)
		} else {
			a.WatermarkLikedAt = ts.UTC()
		}
	}
	return a
}

// SetSpotifyRefreshToken persists the token from the one-time consent flow.
func (s *Store) SetSpotifyRefreshToken(ctx context.Context, accountID int64, token string) error {
	if err := s.q.SetSpotifyRefreshToken(ctx, sqlitegen.SetSpotifyRefreshTokenParams{
		SpotifyRefreshToken: token,
		ID:                  accountID,
	}); err != nil {
		return fmt.Errorf("sqlite.SetSpotifyRefreshToken: %w", err)
	}
	return nil
}

// SetWatermark advances the incremental-sync high-water mark. Callers must
// only do this after a fully successful pass, so an interrupted run
// re-reads rather than skipping likes it never processed.
func (s *Store) SetWatermark(ctx context.Context, accountID int64, at time.Time) error {
	if err := s.q.SetWatermark(ctx, sqlitegen.SetWatermarkParams{
		WatermarkLikedAt: at.UTC().Format(TimeFormat),
		ID:               accountID,
	}); err != nil {
		return fmt.Errorf("sqlite.SetWatermark: %w", err)
	}
	return nil
}

// IsSynced reports whether a track already landed in the playlist.
func (s *Store) IsSynced(ctx context.Context, accountID, trackID int64, playlistID string) (bool, error) {
	n, err := s.q.IsTrackSynced(ctx, sqlitegen.IsTrackSyncedParams{
		AccountID:   accountID,
		DifmTrackID: trackID,
		PlaylistID:  playlistID,
	})
	if err != nil {
		return false, fmt.Errorf("sqlite.IsSynced: %w", err)
	}
	return n != 0, nil
}

// SyncedTrack describes a completed add.
type SyncedTrack struct {
	AccountID      int64
	DifmTrackID    int64
	DifmVoteID     int64
	SpotifyTrackID string
	PlaylistID     string
	Artist, Title  string
	MatchScore     float64
	LikedAt        time.Time
}

// RecordSynced writes the idempotency ledger entry. The unique constraint
// makes a repeated call a no-op rather than an error.
func (s *Store) RecordSynced(ctx context.Context, t SyncedTrack) error {
	if err := s.q.RecordSyncedTrack(ctx, sqlitegen.RecordSyncedTrackParams{
		AccountID:      t.AccountID,
		DifmTrackID:    t.DifmTrackID,
		DifmVoteID:     t.DifmVoteID,
		SpotifyTrackID: t.SpotifyTrackID,
		PlaylistID:     t.PlaylistID,
		Artist:         t.Artist,
		Title:          t.Title,
		MatchScore:     t.MatchScore,
		LikedAt:        t.LikedAt.UTC().Format(TimeFormat),
	}); err != nil {
		return fmt.Errorf("sqlite.RecordSynced: %w", err)
	}
	return nil
}

// CountSynced returns how many tracks have been added for an account.
func (s *Store) CountSynced(ctx context.Context, accountID int64) (int64, error) {
	n, err := s.q.CountSyncedTracks(ctx, accountID)
	if err != nil {
		return 0, fmt.Errorf("sqlite.CountSynced: %w", err)
	}
	return n, nil
}

// ReviewItem is a like that needs a human decision.
type ReviewItem struct {
	AccountID     int64
	DifmTrackID   int64
	DifmVoteID    int64
	Artist, Title string
	DurationSec   int
	DetailsURL    string
	Candidates    []match.Scored
	BestScore     float64
	Reason        string
	Status        string
	LikedAt       time.Time
}

// Enqueue records a like that did not auto-add.
func (s *Store) Enqueue(ctx context.Context, item ReviewItem) error {
	payload, err := json.Marshal(item.Candidates)
	if err != nil {
		return fmt.Errorf("sqlite.Enqueue: encode candidates: %w", err)
	}
	if err := s.q.EnqueueReview(ctx, sqlitegen.EnqueueReviewParams{
		AccountID:      item.AccountID,
		DifmTrackID:    item.DifmTrackID,
		DifmVoteID:     item.DifmVoteID,
		Artist:         item.Artist,
		Title:          item.Title,
		DurationSec:    int64(item.DurationSec),
		DetailsUrl:     item.DetailsURL,
		CandidatesJson: string(payload),
		BestScore:      item.BestScore,
		Reason:         item.Reason,
		LikedAt:        item.LikedAt.UTC().Format(TimeFormat),
	}); err != nil {
		return fmt.Errorf("sqlite.Enqueue: %w", err)
	}
	return nil
}

// ListReview returns queued items with the given status, best-scoring first.
func (s *Store) ListReview(ctx context.Context, accountID int64, status string, limit int) ([]ReviewItem, error) {
	rows, err := s.q.ListReviewQueue(ctx, sqlitegen.ListReviewQueueParams{
		AccountID: accountID,
		Status:    status,
		Limit:     int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite.ListReview: %w", err)
	}
	out := make([]ReviewItem, 0, len(rows))
	for _, r := range rows {
		item := ReviewItem{
			AccountID:   r.AccountID,
			DifmTrackID: r.DifmTrackID,
			DifmVoteID:  r.DifmVoteID,
			Artist:      r.Artist,
			Title:       r.Title,
			DurationSec: int(r.DurationSec),
			DetailsURL:  r.DetailsUrl,
			BestScore:   r.BestScore,
			Reason:      r.Reason,
			Status:      r.Status,
		}
		// A malformed candidates blob must not sink the whole listing —
		// the row's own fields are still useful to a human reviewer.
		_ = json.Unmarshal([]byte(r.CandidatesJson), &item.Candidates)
		if ts, err := time.Parse(TimeFormat, r.LikedAt); err == nil {
			item.LikedAt = ts.UTC()
		}
		out = append(out, item)
	}
	return out, nil
}

// ResolveReview marks a queued item approved or rejected.
//
// Returns whether a row actually matched, for the same reason
// ForgetTrack does: a mistyped track id would otherwise exit zero and
// leave the operator believing something was resolved.
func (s *Store) ResolveReview(ctx context.Context, accountID, trackID int64, status string) (bool, error) {
	n, err := s.q.ResolveReview(ctx, sqlitegen.ResolveReviewParams{
		Status:      status,
		AccountID:   accountID,
		DifmTrackID: trackID,
	})
	if err != nil {
		return false, fmt.Errorf("sqlite.ResolveReview: %w", err)
	}
	return n > 0, nil
}

// GetReviewItem returns a single queued item by DI.fm track id.
func (s *Store) GetReviewItem(ctx context.Context, accountID, trackID int64) (ReviewItem, error) {
	r, err := s.q.GetReviewItem(ctx, sqlitegen.GetReviewItemParams{
		AccountID:   accountID,
		DifmTrackID: trackID,
	})
	if err != nil {
		return ReviewItem{}, fmt.Errorf("sqlite.GetReviewItem(%d): %w", trackID, err)
	}
	item := ReviewItem{
		AccountID:   r.AccountID,
		DifmTrackID: r.DifmTrackID,
		DifmVoteID:  r.DifmVoteID,
		Artist:      r.Artist,
		Title:       r.Title,
		DurationSec: int(r.DurationSec),
		DetailsURL:  r.DetailsUrl,
		BestScore:   r.BestScore,
		Reason:      r.Reason,
		Status:      r.Status,
	}
	_ = json.Unmarshal([]byte(r.CandidatesJson), &item.Candidates)
	if ts, err := time.Parse(TimeFormat, r.LikedAt); err == nil {
		item.LikedAt = ts.UTC()
	}
	return item, nil
}

// CountReview returns how many queue items carry a status. `status`
// previously counted a capped listing, so a queue past the cap reported
// the cap.
func (s *Store) CountReview(ctx context.Context, accountID int64, status string) (int64, error) {
	n, err := s.q.CountReviewQueue(ctx, sqlitegen.CountReviewQueueParams{
		AccountID: accountID,
		Status:    status,
	})
	if err != nil {
		return 0, fmt.Errorf("sqlite.CountReview: %w", err)
	}
	return n, nil
}

// CountActionableReview counts pending items a human could actually act
// on, excluding skipped non-tracks. Those are recorded so nothing is
// lost, but nobody will ever approve a DJ mix.
func (s *Store) CountActionableReview(ctx context.Context, accountID int64) (int64, error) {
	n, err := s.q.CountReviewQueueActionable(ctx, accountID)
	if err != nil {
		return 0, fmt.Errorf("sqlite.CountActionableReview: %w", err)
	}
	return n, nil
}

// RunStats is the outcome of one sync pass.
type RunStats struct {
	Fetched, Added, Queued, Skipped int
	Err                             error
}

// StartRun opens a sync_runs row and returns its id.
func (s *Store) StartRun(ctx context.Context, accountID int64, dryRun bool) (int64, error) {
	var dry int64
	if dryRun {
		dry = 1
	}
	row, err := s.q.StartSyncRun(ctx, sqlitegen.StartSyncRunParams{
		AccountID: accountID,
		StartedAt: s.now(),
		DryRun:    dry,
	})
	if err != nil {
		return 0, fmt.Errorf("sqlite.StartRun: %w", err)
	}
	return row.ID, nil
}

// FinishRun closes the sync_runs row. A failed pass is recorded, not
// discarded — that record is what makes silent failure visible.
func (s *Store) FinishRun(ctx context.Context, runID int64, st RunStats) error {
	var msg string
	if st.Err != nil {
		msg = st.Err.Error()
	}
	if err := s.q.FinishSyncRun(ctx, sqlitegen.FinishSyncRunParams{
		FinishedAt: sql.NullString{String: s.now(), Valid: true},
		Fetched:    int64(st.Fetched),
		Added:      int64(st.Added),
		Queued:     int64(st.Queued),
		Skipped:    int64(st.Skipped),
		Error:      msg,
		ID:         runID,
	}); err != nil {
		return fmt.Errorf("sqlite.FinishRun: %w", err)
	}
	return nil
}

// ForgetTrack drops a single ledger row so the track becomes eligible to
// be re-added. Used by `difmsync resync` to recover from an accidental
// deletion on the Spotify side.
//
// Returns whether a row actually matched. A mistyped track id would
// otherwise appear to succeed and leave the operator believing recovery
// happened when nothing changed.
func (s *Store) ForgetTrack(ctx context.Context, accountID, trackID int64) (bool, error) {
	n, err := s.q.ForgetSyncedTrack(ctx, sqlitegen.ForgetSyncedTrackParams{
		AccountID:   accountID,
		DifmTrackID: trackID,
	})
	if err != nil {
		return false, fmt.Errorf("sqlite.ForgetTrack: %w", err)
	}
	return n > 0, nil
}

// SyncedTrackLikedAt returns when a ledger row's like was recorded, and
// whether the row exists at all.
//
// Callers must read this *before* forgetting the row. It exists so
// `resync --forget` can clear both suppressors: deleting the ledger row
// alone is a no-op whenever the watermark has already moved past the
// like, which by the time anyone reaches for this command it has.
func (s *Store) SyncedTrackLikedAt(ctx context.Context, accountID, trackID int64) (time.Time, bool, error) {
	at, err := s.q.GetSyncedTrackLikedAt(ctx, sqlitegen.GetSyncedTrackLikedAtParams{
		AccountID:   accountID,
		DifmTrackID: trackID,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("sqlite.SyncedTrackLikedAt: %w", err)
	}
	ts, err := time.Parse(TimeFormat, at)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("sqlite.SyncedTrackLikedAt: parse %q: %w", at, err)
	}
	return ts.UTC(), true, nil
}

// ForgetAllTracks empties the ledger for an account. On its own this does
// not duplicate anything: the sync pass also reconciles against the live
// playlist contents before adding.
func (s *Store) ForgetAllTracks(ctx context.Context, accountID int64) error {
	if err := s.q.ForgetAllSyncedTracks(ctx, accountID); err != nil {
		return fmt.Errorf("sqlite.ForgetAllTracks: %w", err)
	}
	return nil
}

// ClearWatermark resets the incremental-sync mark so the next pass reads
// the full like history again.
//
// This is the other half of recovery, and the non-obvious one: the
// watermark filters at *fetch* time, so clearing ledger rows alone is not
// enough to resurrect an old like — it would never be retrieved.
func (s *Store) ClearWatermark(ctx context.Context, accountID int64) error {
	if err := s.q.ClearWatermark(ctx, accountID); err != nil {
		return fmt.Errorf("sqlite.ClearWatermark: %w", err)
	}
	return nil
}

// SyncRun is one recorded pass.
type SyncRun struct {
	ID                              int64
	StartedAt                       string
	FinishedAt                      string
	DryRun                          bool
	Fetched, Added, Queued, Skipped int
	Error                           string
}

// ListRuns returns recent passes, newest first. A failed pass is recorded
// like any other, which is what makes a silently broken sync visible.
func (s *Store) ListRuns(ctx context.Context, accountID int64, limit int) ([]SyncRun, error) {
	rows, err := s.q.ListSyncRuns(ctx, sqlitegen.ListSyncRunsParams{
		AccountID: accountID,
		Limit:     int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite.ListRuns: %w", err)
	}
	out := make([]SyncRun, 0, len(rows))
	for _, r := range rows {
		out = append(out, SyncRun{
			ID:         r.ID,
			StartedAt:  r.StartedAt,
			FinishedAt: r.FinishedAt.String,
			DryRun:     r.DryRun != 0,
			Fetched:    int(r.Fetched),
			Added:      int(r.Added),
			Queued:     int(r.Queued),
			Skipped:    int(r.Skipped),
			Error:      r.Error,
		})
	}
	return out, nil
}
