// Package syncer is the orchestration layer: fetch likes, decide what
// each one is, write the confident ones to Spotify, and queue the rest.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/mjrossi/difm-spotify-sync/internal/store/sqlite"
	"github.com/mjrossi/difm-spotify-sync/pkg/difm"
	"github.com/mjrossi/difm-spotify-sync/pkg/match"
	"github.com/mjrossi/difm-spotify-sync/pkg/spotify"
)

// minInterval floors the sync interval. Below this the jitter
// computation degenerates and the API traffic stops being polite.
const minInterval = time.Minute

// ErrPassIncomplete reports a pass that finished but swallowed at least
// one failure, so the watermark was held back and the affected likes will
// be re-read next pass. Distinguishable from a pass that aborted: the
// writes that did succeed are durable.
var ErrPassIncomplete = errors.New("sync pass incomplete")

// Thresholds partition candidate scores into the three outcomes.
type Thresholds struct {
	Auto   float64 // at or above: added without asking
	Review float64 // at or above: queued for a human; below: recorded as no-match
}

// Engine performs sync passes.
type Engine struct {
	DiFM       *difm.Client
	Spotify    *spotify.Client
	Store      *sqlite.Store
	Account    sqlite.Account
	PlaylistID string
	Thresholds Thresholds
	Log        *slog.Logger
}

// RunOnce performs a single sync pass.
//
// When dryRun is set, every read and every scoring decision still happens
// and the full report is logged, but no Spotify write and no ledger write
// occurs. That is the intended first-run mode: a one-way playlist append
// is tedious to undo by hand.
func (e *Engine) RunOnce(ctx context.Context, dryRun bool) (sqlite.RunStats, error) {
	var stats sqlite.RunStats

	// Re-read the account rather than trusting the copy taken at
	// construction. `difmsync resync` and `difmsync auth` both write to
	// this row while the daemon is running — and since the deployment
	// pins one machine with an internal ticker, the daemon is running
	// whenever an operator reaches for either. A cached copy ignores the
	// operator's reset and then writes the stale value back over it on
	// the next tick, which looks exactly like the escape hatch not
	// working.
	account, err := e.Store.GetAccount(ctx, e.Account.Label)
	if err != nil {
		stats.Err = err
		return stats, fmt.Errorf("reload account: %w", err)
	}
	e.Account = account

	runID, err := e.Store.StartRun(ctx, account.ID, dryRun)
	if err != nil {
		return stats, err
	}
	defer func() {
		// Detached from ctx. On SIGTERM mid-pass ctx is already canceled,
		// and closing the row with a canceled context fails — leaving a
		// run that never finishes and a phantom "in flight" in `difmsync
		// status`. Shutdown mid-pass is the normal path for a deploy, not
		// an edge case.
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if ferr := e.Store.FinishRun(fctx, runID, stats); ferr != nil {
			e.Log.Error("could not close sync run", "err", ferr)
		}
	}()

	// passClean gates the watermark, and every error this pass swallows in
	// order to keep going must clear it.
	//
	// CLAUDE.md's invariant 2 requires a partial failure to leave the
	// watermark where it was. "Partial" includes per-item failures, not
	// only failures that abort the pass: the watermark filters at fetch
	// time, so advancing past a like that was never durably recorded means
	// nothing ever re-reads it. It is gone, and `resync` cannot recover it
	// because `resync` clears suppressors for likes the ledger knows about
	// and this one was never recorded anywhere.
	passClean := true
	fail := func(err error) {
		passClean = false
		if stats.Err == nil {
			stats.Err = err
		}
	}

	// highWater advances only for likes that reached a durable terminal
	// state — recorded in the ledger, or queued for review. Computing it
	// from the input instead would move it past likes that failed.
	highWater := account.WatermarkLikedAt
	advance := func(t time.Time) {
		if t.After(highWater) {
			highWater = t
		}
	}

	// record queues a like that is not being added to Spotify — skipped,
	// low-confidence, or unmatched — and reports whether the pass may count
	// it. Three arms of the switch below did this identically.
	//
	// fail and advance stay closures in this function's scope and are only
	// captured here, never returned through. That is deliberate: the
	// ordering they encode is the invariant, and putting either behind a
	// return value is how a later edit reorders them without noticing.
	//
	// The caller increments its own counter on true, after the write rather
	// than before it, so a pass whose queue writes all failed does not
	// report them as recorded.
	record := func(like difm.Track, candidates []match.Scored, reason, failMsg string) bool {
		if dryRun {
			return true
		}
		if err := e.enqueue(ctx, like, candidates, reason); err != nil {
			e.Log.Error(failMsg, "track", like.Artist+" - "+like.Title, "err", err)
			fail(err)
			return false
		}
		advance(like.LikedAt)
		return true
	}

	likes, err := e.DiFM.ListLikedTracks(ctx, account.WatermarkLikedAt)
	switch {
	case errors.Is(err, difm.ErrDropped):
		// Some records could not be read. The ones that could are real and
		// worth syncing, so the prefix *is* processed — but a dropped
		// record is a like that reached no durable state anywhere, so the
		// pass is not clean and the watermark stays put. Without this the
		// engine would advance past a like it never saw, and nothing would
		// re-read it.
		e.Log.Error("some likes could not be read; watermark will be held", "err", err)
		fail(err)
	case err != nil:
		// Includes difm.ErrTruncated, which carries a partial prefix. The
		// prefix is deliberately not processed: a pass that cannot see all
		// the likes must not advance past the ones it did see.
		stats.Err = err
		return stats, err
	}
	stats.Fetched = len(likes)
	e.Log.Info("fetched likes", "count", len(likes), "since", account.WatermarkLikedAt, "dry_run", dryRun)

	// Reconcile against the live playlist, not just the ledger. The two can
	// legitimately disagree — a restored database, a `resync --forget`, or a
	// track added by hand — and trusting the ledger alone would duplicate in
	// every one of those cases. One paginated read per pass.
	inPlaylist, err := e.Spotify.PlaylistTrackIDs(ctx, e.PlaylistID)
	if err != nil {
		// Aborting rather than degrading to ledger-only dedupe. The whole
		// premise of invariant 3 is that the ledger may be wrong, and the
		// cases it is wrong in — restored DB, cleared ledger, manual add —
		// are exactly the ones this read covers. Falling back to the
		// ledger in the one situation where the ledger cannot be trusted
		// duplicates silently, and an add-only sync cannot undo that.
		stats.Err = err
		return stats, fmt.Errorf("reconcile playlist contents: %w", err)
	}

	var (
		pendingIDs   []string
		pendingLedge []sqlite.SyncedTrack
	)

	for _, like := range likes {
		// DJ mixes and mix-show episodes have no Spotify analog.
		// Searching for them yields nothing or, worse, a same-titled
		// unrelated track — so record and move on.
		if like.Skip {
			e.Log.Debug("skipping non-track", "title", like.Title, "reason", like.SkipReason)
			if record(like, nil, sqlite.ReasonSkipped, "could not queue skipped track") {
				stats.Skipped++
			}
			continue
		}

		already, err := e.Store.IsSynced(ctx, account.ID, like.TrackID, e.PlaylistID)
		if err != nil {
			stats.Err = err
			return stats, err
		}
		if already {
			advance(like.LikedAt)
			continue
		}

		candidates, err := e.Spotify.Search(ctx, like.Artist, like.Title, like.DurationSec, like.ISRC)
		if err != nil {
			// A rate limit or a dead grant affects every remaining track,
			// so there is nothing to gain by continuing and real harm in
			// hammering a throttled API.
			if errors.Is(err, spotify.ErrRateLimited) || errors.Is(err, spotify.ErrUnauthorized) {
				stats.Err = err
				return stats, fmt.Errorf("spotify search: %w", err)
			}
			// Anything else: one unsearchable track must not abort the
			// pass, but it is a failure, not a verdict. Queuing it as
			// `no_match` would record "Spotify does not have this" when
			// what actually happened is "we could not ask".
			e.Log.Error("spotify search failed", "artist", like.Artist, "title", like.Title, "err", err)
			fail(err)
			continue
		}

		best, ok := bestOf(candidates)
		switch {
		case ok && best.Score >= e.Thresholds.Auto && inPlaylist[best.ID]:
			// Already on Spotify but missing from the ledger. Record it so
			// later passes are cheap, and do not add it a second time.
			e.Log.Info("already in playlist; recording without re-adding",
				"track", like.Artist+" - "+like.Title)
			if dryRun {
				continue
			}
			// Queued rather than written here. inPlaylist is also set by
			// the auto-add branch below for tracks that are only *pending*
			// a POST, so this branch is reachable for a track that is not
			// in the playlist yet — and writing its ledger row now would
			// put the ledger ahead of the Spotify write, inverting the
			// first invariant. Deferring keeps every ledger row behind the
			// add, in the one transaction with the watermark.
			pendingLedge = append(pendingLedge, e.ledgerEntry(account.ID, like, best))

		case ok && best.Score >= e.Thresholds.Auto:
			e.Log.Info("match",
				"difm", like.Artist+" - "+like.Title,
				"spotify", best.Artist+" - "+best.Title,
				"score", roundTo(best.Score, 3))
			pendingIDs = append(pendingIDs, best.ID)
			pendingLedge = append(pendingLedge, e.ledgerEntry(account.ID, like, best))
			// Mark it present immediately. Two DI.fm likes can resolve to
			// one Spotify track — the same recording liked on two channels,
			// or two DI.fm ids for the same release — and without this the
			// set read at the top of the pass never learns about anything
			// added during the pass, so both copies are posted. The ledger
			// does not catch it either, being keyed on the DI.fm track id.
			inPlaylist[best.ID] = true

		case ok && best.Score >= e.Thresholds.Review:
			e.Log.Info("queued for review",
				"track", like.Artist+" - "+like.Title,
				"best", roundTo(best.Score, 3), "why", best.Why)
			if record(like, candidates, sqlite.ReasonLowConfidence, "could not queue for review") {
				stats.Queued++
			}

		default:
			e.Log.Info("no usable match", "track", like.Artist+" - "+like.Title)
			if record(like, candidates, sqlite.ReasonNoMatch, "could not record unmatched track") {
				stats.Queued++
			}
		}
	}

	if dryRun {
		e.Log.Info("dry run complete — nothing written",
			"would_add", len(pendingIDs), "would_queue", stats.Queued, "skipped", stats.Skipped)
		stats.Added = len(pendingIDs)
		return stats, nil
	}

	if len(pendingIDs) > 0 {
		// Re-read the playlist immediately before writing. The
		// reconciliation at the top of the pass is minutes and N searches
		// old by now, and `difmsync review --approve` writes to the same
		// playlist from another process — so a track can arrive in the
		// window and this add would duplicate it. Add-only sync never
		// removes the duplicate, so the window is worth one API call.
		current, err := e.Spotify.PlaylistTrackIDs(ctx, e.PlaylistID)
		if err != nil {
			// Same reasoning as the reconciliation read: degrading to
			// "add anyway" is exactly wrong in the cases this exists for.
			stats.Err = err
			return stats, fmt.Errorf("re-read playlist before add: %w", err)
		}
		fresh := pendingIDs[:0:0]
		for _, id := range pendingIDs {
			if current[id] {
				e.Log.Info("track arrived in playlist during the pass; not re-adding", "spotify_id", id)
				continue
			}
			fresh = append(fresh, id)
		}
		if len(fresh) > 0 {
			if err := e.Spotify.AddToPlaylist(ctx, e.PlaylistID, fresh); err != nil {
				stats.Err = err
				return stats, err
			}
		}
		// Counted here, not at match time: sync_runs.added is the primary
		// observability surface on a headless box and should report what
		// actually landed in the playlist.
		stats.Added = len(fresh)
	}

	// Every pending ledger row is now safe to advance over: the track it
	// references is in the playlist, whether it was added just now or
	// observed there during reconciliation. The rows themselves are
	// written below, in one transaction with the watermark.
	for _, entry := range pendingLedge {
		advance(entry.LikedAt)
	}

	// A canceled context past this point means shutdown arrived mid-pass.
	// Stop before the watermark: the pass is incomplete by definition.
	if err := ctx.Err(); err != nil {
		stats.Err = err
		return stats, err
	}

	// Ledger writes follow the successful add, never precede it: a crash
	// between the two must re-add, not silently skip. The watermark rides
	// in the same transaction so it can never land ahead of the ledger.
	advanceMark := passClean && highWater.After(account.WatermarkLikedAt)
	if len(pendingLedge) > 0 || advanceMark {
		if err := e.Store.InTx(ctx, func(tx *sqlite.Store) error {
			for _, entry := range pendingLedge {
				if err := tx.RecordSynced(ctx, entry); err != nil {
					return err
				}
			}
			if advanceMark {
				return tx.SetWatermark(ctx, account.ID, highWater)
			}
			return nil
		}); err != nil {
			stats.Err = err
			return stats, err
		}
		if advanceMark {
			e.Account.WatermarkLikedAt = highWater
		}
	}

	if !passClean {
		e.Log.Warn("pass completed with failures; watermark held back",
			"watermark", account.WatermarkLikedAt, "err", stats.Err)
		// Returned as an error so a one-shot `difmsync sync` — from cron,
		// from CI, from a shell — exits non-zero. Reporting success for a
		// pass that dropped likes makes sync_runs.error the only place the
		// failure exists, which nothing outside this box ever reads.
		// Loop deliberately continues past it; see there.
		return stats, fmt.Errorf("%w: %w", ErrPassIncomplete, stats.Err)
	}

	e.Log.Info("sync complete",
		"added", stats.Added, "queued", stats.Queued, "skipped", stats.Skipped)
	return stats, nil
}

// ledgerEntry builds the idempotency ledger row for a like that resolved
// to a Spotify track.
//
// Both auto-add branches produce the identical row — one for a track this
// pass posts, one for a track reconciliation already found in the
// playlist — and they must stay identical. These rows are what IsSynced
// reads on later passes, so a field populated on one path and not the
// other makes a track look unsynced forever and re-search every tick.
func (e *Engine) ledgerEntry(accountID int64, like difm.Track, best match.Scored) sqlite.SyncedTrack {
	return sqlite.SyncedTrack{
		AccountID:      accountID,
		DifmTrackID:    like.TrackID,
		DifmVoteID:     like.VoteID,
		SpotifyTrackID: best.ID,
		PlaylistID:     e.PlaylistID,
		Artist:         like.Artist,
		Title:          like.Title,
		MatchScore:     best.Score,
		LikedAt:        like.LikedAt,
	}
}

func (e *Engine) enqueue(ctx context.Context, like difm.Track, candidates []match.Scored, reason string) error {
	var best float64
	if b, ok := bestOf(candidates); ok {
		best = b.Score
	}
	return e.Store.Enqueue(ctx, sqlite.ReviewItem{
		AccountID:   e.Account.ID,
		DifmTrackID: like.TrackID,
		DifmVoteID:  like.VoteID,
		Artist:      like.Artist,
		Title:       like.Title,
		DurationSec: like.DurationSec,
		DetailsURL:  like.DetailsURL,
		Candidates:  topN(candidates, 5),
		BestScore:   best,
		Reason:      reason,
		LikedAt:     like.LikedAt,
	})
}

// Loop runs passes on an interval until ctx is canceled. The first tick
// is jittered so multiple deployments don't stampede the APIs together.
func (e *Engine) Loop(ctx context.Context, interval time.Duration, dryRun bool) error {
	// The interval is operator-supplied via --interval/DIFMSYNC_INTERVAL.
	// Two separate reasons to floor it: rand.Int64N panics outright below
	// 4ns, and anything under a minute stops being polite to a private
	// API. The floor is set by the second, which is why a deliberate
	// --interval=30s is overridden rather than honored — it warns.
	if interval < minInterval {
		e.Log.Warn("interval too small; clamping",
			"requested", interval, "using", minInterval)
		interval = minInterval
	}
	jitter := time.Duration(rand.Int64N(int64(interval / 4)))
	e.Log.Info("starting sync loop", "interval", interval, "first_run_in", jitter)

	timer := time.NewTimer(jitter)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			// A clean shutdown mid-pass is not an error: the watermark
			// simply hasn't advanced, so the next boot re-reads.
			if errors.Is(ctx.Err(), context.Canceled) {
				e.Log.Info("sync loop stopped")
				return nil
			}
			return ctx.Err()
		case <-timer.C:
			if _, err := e.RunOnce(ctx, dryRun); err != nil {
				// Keep looping: a transient API failure should not kill
				// a long-running daemon. ErrPassIncomplete in particular
				// is self-correcting — the watermark held, so the next
				// tick re-reads whatever was missed.
				e.Log.Error("sync pass failed", "err", err)
			}
			timer.Reset(interval)
		}
	}
}

func bestOf(candidates []match.Scored) (match.Scored, bool) {
	if len(candidates) == 0 {
		return match.Scored{}, false
	}
	return candidates[0], true // Search returns them sorted descending
}

func topN(candidates []match.Scored, n int) []match.Scored {
	if len(candidates) > n {
		return candidates[:n]
	}
	return candidates
}

func roundTo(f float64, places int) string {
	return fmt.Sprintf("%.*f", places, f)
}
