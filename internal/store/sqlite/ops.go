package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
	// Explicit rather than mapping the zero row: toAccount on a failed read
	// happens to be harmless today only because an empty watermark string
	// skips its parse. That is a coincidence, not a guarantee.
	if err != nil {
		return Account{}, opErr("EnsureAccount", err)
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
	return opErr("SetSpotifyRefreshToken", s.q.SetSpotifyRefreshToken(ctx, sqlitegen.SetSpotifyRefreshTokenParams{
		SpotifyRefreshToken: token,
		ID:                  accountID,
	}))
}

// HasSpotifyRefreshToken reports whether consent has been stored for the
// account, without reading the token itself.
//
// The caller is the daemon's consent wait, which polls this so that a
// token written by any other entry point — `difmsync auth`, `auth
// --manual` in a sidecar, a restored database — ends the wait. Before
// this existed the daemon only ever noticed consent completed through
// its own listener, so authorizing out of band stored the token and left
// the daemon waiting on a URL nobody was going to open.
func (s *Store) HasSpotifyRefreshToken(ctx context.Context, accountID int64) (bool, error) {
	n, err := s.q.CountSpotifyRefreshToken(ctx, accountID)
	return n > 0, opErr("HasSpotifyRefreshToken", err)
}

// SetWatermark advances the incremental-sync high-water mark. Callers must
// only do this after a fully successful pass, so an interrupted run
// re-reads rather than skipping likes it never processed.
func (s *Store) SetWatermark(ctx context.Context, accountID int64, at time.Time) error {
	return opErr("SetWatermark", s.q.SetWatermark(ctx, sqlitegen.SetWatermarkParams{
		WatermarkLikedAt: at.UTC().Format(TimeFormat),
		ID:               accountID,
	}))
}

// IsSynced reports whether a track already landed in the playlist.
func (s *Store) IsSynced(ctx context.Context, accountID, trackID int64, playlistID string) (bool, error) {
	n, err := s.q.IsTrackSynced(ctx, sqlitegen.IsTrackSyncedParams{
		AccountID:   accountID,
		DifmTrackID: trackID,
		PlaylistID:  playlistID,
	})
	return n != 0, opErr("IsSynced", err)
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
	return opErr("RecordSynced", s.q.RecordSyncedTrack(ctx, sqlitegen.RecordSyncedTrackParams{
		AccountID:      t.AccountID,
		DifmTrackID:    t.DifmTrackID,
		DifmVoteID:     t.DifmVoteID,
		SpotifyTrackID: t.SpotifyTrackID,
		PlaylistID:     t.PlaylistID,
		Artist:         t.Artist,
		Title:          t.Title,
		MatchScore:     t.MatchScore,
		LikedAt:        t.LikedAt.UTC().Format(TimeFormat),
	}))
}

// CountSynced returns how many tracks have been added for an account.
func (s *Store) CountSynced(ctx context.Context, accountID int64) (int64, error) {
	n, err := s.q.CountSyncedTracks(ctx, accountID)
	return n, opErr("CountSynced", err)
}

// opErr tags a generated query's error with the store method that ran it.
// sqlc errors name neither, so without this a failure reads as a bare
// "sql: no rows in result set" with nothing to locate it by.
func opErr(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("sqlite.%s: %w", op, err)
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

// toReviewItem flattens a queue row, the single place the mapping lives.
//
// ListReview and GetReviewItem read the same sqlc row type, so a column
// added to review_queue has to reach both or neither. Two hand-written
// copies make "neither" easy to miss: `review` lists through one and
// `review --approve` decides through the other, so a field that lands on
// only one path means the operator approves against something the
// listing never showed them.
//
// A malformed candidates blob is tolerated rather than fatal — the row's
// own fields are still useful to a human reviewer — and so is an
// unparseable liked_at, which leaves LikedAt zero.
func toReviewItem(r sqlitegen.ReviewQueue) ReviewItem {
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
	return item
}

// Enqueue records a like that did not auto-add.
func (s *Store) Enqueue(ctx context.Context, item ReviewItem) error {
	payload, err := json.Marshal(item.Candidates)
	if err != nil {
		return fmt.Errorf("sqlite.Enqueue: encode candidates: %w", err)
	}
	return opErr("Enqueue", s.q.EnqueueReview(ctx, sqlitegen.EnqueueReviewParams{
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
	}))
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
		out = append(out, toReviewItem(r))
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
	return n > 0, opErr("ResolveReview", err)
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
	return toReviewItem(r), nil
}

// CountReview returns how many queue items carry a status. `status`
// previously counted a capped listing, so a queue past the cap reported
// the cap.
func (s *Store) CountReview(ctx context.Context, accountID int64, status string) (int64, error) {
	n, err := s.q.CountReviewQueue(ctx, sqlitegen.CountReviewQueueParams{
		AccountID: accountID,
		Status:    status,
	})
	return n, opErr("CountReview", err)
}

// CountActionableReview counts pending items a human could actually act
// on, excluding skipped non-tracks. Those are recorded so nothing is
// lost, but nobody will ever approve a DJ mix.
func (s *Store) CountActionableReview(ctx context.Context, accountID int64) (int64, error) {
	n, err := s.q.CountReviewQueueActionable(ctx, accountID)
	return n, opErr("CountActionableReview", err)
}

// RunErrorKind categorizes why a pass failed, for the one consumer that
// is not allowed to see the error text: the status endpoints. The
// engine picks the value from its own sentinels, so a kind is always a
// string this code wrote, never one an API returned.
type RunErrorKind string

const (
	// KindDiFMUnauthorized: DI.fm rejected the API key.
	KindDiFMUnauthorized RunErrorKind = "difm_unauthorized"
	// KindSpotifyGrantRevoked: the token endpoint refused the refresh
	// token; the daemon has cleared it and is waiting for consent.
	KindSpotifyGrantRevoked RunErrorKind = "spotify_grant_revoked"
	// KindRateLimited: either API answered 429; the next pass is delayed.
	KindRateLimited RunErrorKind = "rate_limited"
	// KindIncomplete: the pass finished but swallowed at least one
	// failure, so the watermark was held (syncer.ErrPassIncomplete).
	KindIncomplete RunErrorKind = "incomplete"
	// KindError: failed for a reason with no more specific kind, including
	// a pass cut short by shutdown.
	KindError RunErrorKind = "error"
)

// Known reports whether k is a value this package defines. The column
// is published by the status endpoints, so FinishRun refuses to write
// anything else: a caller that smuggled error text in through the kind
// would reopen exactly the channel the column exists to close. It is
// also the read-side check: a row a process did not write itself — a
// restore, a hand edit — gets the same exclusion before it is served.
func (k RunErrorKind) Known() bool {
	switch k {
	case "", KindDiFMUnauthorized, KindSpotifyGrantRevoked, KindRateLimited, KindIncomplete, KindError:
		return true
	}
	return false
}

// RunStats is the outcome of one sync pass.
type RunStats struct {
	Fetched, Added, Queued, Skipped int
	Err                             error
	// Kind is set alongside Err by the engine. Empty with a non-nil Err
	// is a bug there, not a state the store interprets. Rows written
	// before migration 0002 read back as empty; that is the only
	// legitimate source of an empty kind on a failed run.
	Kind RunErrorKind
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
	return row.ID, opErr("StartRun", err)
}

// FinishRun closes the sync_runs row. A failed pass is recorded, not
// discarded — that record is what makes silent failure visible.
func (s *Store) FinishRun(ctx context.Context, runID int64, st RunStats) error {
	var msg string
	if st.Err != nil {
		msg = st.Err.Error()
	}
	kind := st.Kind
	if !kind.Known() {
		// Fail safe — write KindError rather than the smuggled value —
		// but not silently: this column is published by the status
		// endpoints, so a caller passing error text through Kind is a
		// bug there worth surfacing, not a state to pass through.
		s.log.Warn("unknown error kind; recording as error",
			"kind", string(st.Kind))
		kind = KindError
	}
	return opErr("FinishRun", s.q.FinishSyncRun(ctx, sqlitegen.FinishSyncRunParams{
		FinishedAt: sql.NullString{String: s.now(), Valid: true},
		Fetched:    int64(st.Fetched),
		Added:      int64(st.Added),
		Queued:     int64(st.Queued),
		Skipped:    int64(st.Skipped),
		Error:      msg,
		ErrorKind:  string(kind),
		ID:         runID,
	}))
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
	return n > 0, opErr("ForgetTrack", err)
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
	return opErr("ForgetAllTracks", s.q.ForgetAllSyncedTracks(ctx, accountID))
}

// ClearWatermark resets the incremental-sync mark so the next pass reads
// the full like history again.
//
// This is the other half of recovery, and the non-obvious one: the
// watermark filters at *fetch* time, so clearing ledger rows alone is not
// enough to resurrect an old like — it would never be retrieved.
func (s *Store) ClearWatermark(ctx context.Context, accountID int64) error {
	return opErr("ClearWatermark", s.q.ClearWatermark(ctx, accountID))
}

// BackupTo writes a consistent snapshot of the database to dest.
//
// VACUUM INTO rather than a file copy: the database runs in WAL mode, so
// copying the file while a pass is writing can capture a torn state that
// looks valid until the moment it is restored. It is also pure Go through
// the modernc driver, which is what lets this run inside the distroless
// image — there is no sqlite3 binary and no shell in there.
//
// SQLite refuses a dest that already exists, and that refusal is kept
// rather than papered over: the file it would overwrite is the only copy
// of a Spotify refresh token often enough to matter.
//
// VACUUM cannot run inside a transaction. The pool is capped at one
// connection, so this serializes against a running pass rather than
// racing it.
func (s *Store) BackupTo(ctx context.Context, dest string) error {
	if s.inTx {
		return errors.New("sqlite.BackupTo: cannot run inside a transaction")
	}
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", dest); err != nil {
		return fmt.Errorf("sqlite.BackupTo(%q): %w", dest, err)
	}
	return nil
}

// SnapshotTo writes a verified snapshot to dest: staged in a private
// directory alongside it, permissions restricted before anything is
// published, reopened and checked for the account row, and only then
// renamed into place.
//
// Lives here rather than in the backup command because the daemon takes
// scheduled snapshots through the same steps, and two snapshot paths is
// how one of them silently loses the staging directory.
func (s *Store) SnapshotTo(ctx context.Context, dest, verifyLabel string) error {
	// Checked before the write so the refusal reads as an instruction
	// rather than as SQLite's "SQL logic error: output file already
	// exists (1)". VACUUM INTO refuses an existing path on its own; this
	// only says so usefully. Overwriting is not offered: the file in the
	// way is often the only copy of a refresh token.
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("%s already exists — refusing to overwrite it "+
			"(it may be the only copy of a refresh token); "+
			"pick another --to, or move the existing file away first", dest)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("checking the backup destination %s: %w", dest, err)
	}
	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return fmt.Errorf("creating the backup directory: %w", err)
	}

	// Staged in a private directory and renamed into place, rather than
	// written straight to dest. Two reasons, both of which bit the
	// straightforward version:
	//
	//   - VACUUM INTO does not clean up after itself. A failure partway —
	//     a full volume is the realistic one, since the nightly backup
	//     writes to the same volume as the database — leaves its partial
	//     output behind. At the destination that is a truncated file with
	//     a plausible dated name, which is exactly what a later restore
	//     would copy over the live database.
	//   - The file is created with the process umask (0644 on a default
	//     setup) and can only be chmod'd once VACUUM returns, so the
	//     refresh token would be world-readable for however long the copy
	//     takes. MkdirTemp creates the staging directory 0700, which
	//     closes that window at the directory instead.
	//
	// Same parent as dest, so the rename stays on one filesystem.
	stage, err := os.MkdirTemp(parent, ".difmsync-backup-")
	if err != nil {
		return fmt.Errorf("creating a staging directory next to %s: %w", dest, err)
	}
	// Removes the staging directory and anything left in it on every path
	// out, so a failed snapshot leaves nothing behind at all.
	defer func() { _ = os.RemoveAll(stage) }()

	tmp := filepath.Join(stage, "difmsync.db")
	if err := s.BackupTo(ctx, tmp); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return fmt.Errorf("restricting permissions on the snapshot: %w", err)
	}

	// Verify by reopening the snapshot and reading the account out of it.
	// Reopening rather than trusting the write is the point: it proves
	// the result is a database that opens and holds the row that matters,
	// not just bytes that landed.
	//
	// A backup without that row is not a backup, and saying so is the
	// whole reason for checking. Restoring one means writing it *over*
	// the live database, so a confident success message on an empty file
	// is the worst outcome available here. Verifying before the rename
	// means an unusable snapshot never reaches the destination to be
	// mistaken for a good one later.
	if err := verifySnapshot(ctx, tmp, dest, verifyLabel); err != nil {
		return fmt.Errorf("%w — check --db-path points at the database you meant", err)
	}

	// Publish only what has been verified. The check at the top is
	// advisory rather than a lock — rename replaces — but what it guards
	// against is running the command twice by hand, and what matters is
	// the invariant that survives either way: nothing unverified is ever
	// written to dest.
	if err := os.Rename(tmp, dest); err != nil {
		return fmt.Errorf("moving the verified snapshot to %s: %w", dest, err)
	}
	return nil
}

// verifySnapshot opens the snapshot at path and confirms it carries the
// account.
//
// Deliberately no Migrate: this must read what was written, not repair it
// into looking valid.
//
// dest is reported rather than path because the two differ by design and
// only one of them survives: path is inside the staging directory, which
// the deferred RemoveAll deletes on exactly the failure paths that
// produce these messages. Naming it sent operators looking for a file
// that no longer exists.
func verifySnapshot(ctx context.Context, path, dest, label string) error {
	store, err := Open(path)
	if err != nil {
		return fmt.Errorf("the snapshot staged for %s does not open as a database: %w", dest, err)
	}
	defer func() { _ = store.Close() }()

	if _, err := store.GetAccount(ctx, label); err != nil {
		return fmt.Errorf("the snapshot staged for %s has no %q account row: %w", dest, label, err)
	}
	return nil
}

// SyncRun is one recorded pass.
//
// Untagged, like every other store type. This was briefly tagged for
// JSON, back when status.Report embedded it directly; serving a store
// struct is what published the Error column to an unauthenticated
// endpoint. internal/status copies it into its own status.Run instead —
// the JSON shape belongs to the operator surface, not here.
type SyncRun struct {
	ID                              int64
	StartedAt                       string
	FinishedAt                      string
	DryRun                          bool
	Fetched, Added, Queued, Skipped int
	Error                           string
	ErrorKind                       RunErrorKind
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
			ErrorKind:  RunErrorKind(r.ErrorKind),
		})
	}
	return out, nil
}

// PruneRuns deletes finished runs that started before the cutoff, except
// the newest keep rows. The floor exists because the health rule reads a
// fixed window of rows (status.HealthScanLimit) and must never lose one
// to housekeeping; the unfinished-row exclusion is what makes it safe to
// call from inside a pass whose own row is still open. It also keeps a
// row a killed pass never closed — one per hard crash, left alone rather
// than guessed at. A negative keep is unlimited (SQLite LIMIT -1), so
// callers pass a constant. Returns the count.
func (s *Store) PruneRuns(ctx context.Context, accountID int64, before time.Time, keep int) (int64, error) {
	n, err := s.q.PruneSyncRuns(ctx, sqlitegen.PruneSyncRunsParams{
		AccountID:   accountID,
		StartedAt:   before.UTC().Format(TimeFormat),
		AccountID_2: accountID,
		Limit:       int64(keep),
	})
	return n, opErr("PruneRuns", err)
}
