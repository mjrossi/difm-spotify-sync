package syncer_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/mjrossi/difm-spotify-sync/internal/store/sqlite"
	"github.com/mjrossi/difm-spotify-sync/internal/syncer"
	"github.com/mjrossi/difm-spotify-sync/pkg/spotify"
)

var (
	feb = time.Date(2026, 2, 28, 12, 0, 0, 0, time.UTC)
	mar = time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)
)

func aLike(id int64, artist, title string, secs int, at time.Time) like {
	return like{VoteID: id * 10, TrackID: id, Artist: artist, Title: title, Length: secs, LikedAt: at}
}

func TestRunOnce_AddsConfidentMatches(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, []like{aLike(1, "DJ Rax", "Air Race (Spiritchaser Remix)", 480, feb)})
	h.searchResult["Air Race"] = []spotifyTrack{
		{ID: "sp1", Artist: "DJ Rax", Title: "Air Race - Spiritchaser Remix", Seconds: 480},
	}

	stats, err := h.Engine.RunOnce(ctx, false)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Added != 1 || stats.Queued != 0 {
		t.Errorf("stats = %+v, want 1 added, 0 queued", stats)
	}
	if len(h.Added) != 1 || h.Added[0] != "spotify:track:sp1" {
		t.Errorf("added to Spotify = %v, want [spotify:track:sp1]", h.Added)
	}
	if n := h.ledgerCount(t); n != 1 {
		t.Errorf("ledger rows = %d, want 1", n)
	}
	if got := h.reload(t).WatermarkLikedAt; !got.Equal(feb) {
		t.Errorf("watermark = %s, want %s", got, feb)
	}
}

// TestRunOnce_SpotifyFailureLeavesNoState is the central ordering
// invariant: Spotify is written first, the ledger second, the watermark
// last. If the Spotify write fails, neither of the other two may have
// happened — otherwise the next pass believes the track is done and skips
// it forever.
func TestRunOnce_SpotifyFailureLeavesNoState(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, []like{aLike(1, "DJ Rax", "Air Race (Spiritchaser Remix)", 480, feb)})
	h.searchResult["Air Race"] = []spotifyTrack{
		{ID: "sp1", Artist: "DJ Rax", Title: "Air Race - Spiritchaser Remix", Seconds: 480},
	}
	h.failAdd = true

	if _, err := h.Engine.RunOnce(ctx, false); err == nil {
		t.Fatal("expected an error when the Spotify write fails")
	}
	if n := h.ledgerCount(t); n != 0 {
		t.Errorf("ledger rows = %d, want 0 — the ledger must not run ahead of Spotify", n)
	}
	if got := h.reload(t).WatermarkLikedAt; !got.IsZero() {
		t.Errorf("watermark = %s, want zero — a failed pass must not advance it", got)
	}

	// And the failure must be recorded, not swallowed: an unrecorded
	// failure is how a broken sync goes unnoticed on a headless box.
	runs, err := h.Store.ListRuns(ctx, h.Engine.Account.ID, 5)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("recorded %d runs, want 1", len(runs))
	}
	if runs[0].Error == "" {
		t.Error("the failed pass was recorded with an empty error")
	}
	if runs[0].FinishedAt == "" {
		t.Error("the failed pass was never closed out")
	}
}

// A retry after the failure must still add the track — proving the failed
// pass left it eligible rather than silently consumed.
func TestRunOnce_RetryAfterFailureStillAdds(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, []like{aLike(1, "DJ Rax", "Air Race (Spiritchaser Remix)", 480, feb)})
	h.searchResult["Air Race"] = []spotifyTrack{
		{ID: "sp1", Artist: "DJ Rax", Title: "Air Race - Spiritchaser Remix", Seconds: 480},
	}

	h.failAdd = true
	_, _ = h.Engine.RunOnce(ctx, false)

	h.failAdd = false
	stats, err := h.Engine.RunOnce(ctx, false)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if stats.Added != 1 {
		t.Errorf("retry added %d, want 1 — the track must survive a failed pass", stats.Added)
	}
	if n := h.ledgerCount(t); n != 1 {
		t.Errorf("ledger rows = %d, want 1", n)
	}
}

func TestRunOnce_DryRunWritesNothing(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, []like{aLike(1, "DJ Rax", "Air Race (Spiritchaser Remix)", 480, feb)})
	h.searchResult["Air Race"] = []spotifyTrack{
		{ID: "sp1", Artist: "DJ Rax", Title: "Air Race - Spiritchaser Remix", Seconds: 480},
	}

	stats, err := h.Engine.RunOnce(ctx, true)
	if err != nil {
		t.Fatalf("RunOnce(dry): %v", err)
	}
	if stats.Added != 1 {
		t.Errorf("stats.Added = %d, want 1 — a dry run still reports what it would do", stats.Added)
	}
	if len(h.Added) != 0 {
		t.Errorf("wrote %v to Spotify during a dry run", h.Added)
	}
	if n := h.ledgerCount(t); n != 0 {
		t.Errorf("ledger rows = %d, want 0 during a dry run", n)
	}
	if got := h.reload(t).WatermarkLikedAt; !got.IsZero() {
		t.Errorf("watermark = %s, want zero — a dry run must not advance it", got)
	}
	if len(h.pending(t)) != 0 {
		t.Error("dry run enqueued review items")
	}
}

func TestRunOnce_SkipsNonTracksWithoutSearching(t *testing.T) {
	ctx := context.Background()
	mix := like{VoteID: 10, TrackID: 1, Artist: "A DJ", Title: "Long Set", Length: 3600, LikedAt: feb, Mix: true}
	ep := like{VoteID: 20, TrackID: 2, Artist: "Host", Title: "Episode 12", Length: 3600, LikedAt: feb, Episode: true}
	long := like{VoteID: 30, TrackID: 3, Artist: "Someone", Title: "Marathon", Length: 5400, LikedAt: feb}
	h := newHarness(t, []like{mix, ep, long})

	stats, err := h.Engine.RunOnce(ctx, false)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Skipped != 3 {
		t.Errorf("skipped = %d, want 3 (DJ mix, episode, over-length)", stats.Skipped)
	}
	if stats.Added != 0 || len(h.Added) != 0 {
		t.Errorf("added %v, want nothing — DJ sets have no Spotify analog", h.Added)
	}
	// Searching for an hour-long set wastes calls and risks matching an
	// unrelated track with the same name.
	if h.SearchCount != 0 {
		t.Errorf("issued %d searches for non-tracks, want 0", h.SearchCount)
	}
	// Skipped is not dropped: it must still be visible for review.
	items := h.pending(t)
	if len(items) != 3 {
		t.Fatalf("review queue has %d items, want 3 — nothing may be silently dropped", len(items))
	}
	for _, it := range items {
		if it.Reason != sqlite.ReasonSkipped {
			t.Errorf("track %d reason = %q, want %q", it.DifmTrackID, it.Reason, sqlite.ReasonSkipped)
		}
	}
}

func TestRunOnce_RoutesByThreshold(t *testing.T) {
	ctx := context.Background()
	// Same title, wrong artist: lands above no-match but below auto-add.
	h := newHarness(t, []like{aLike(1, "Elements Of Life", "Live Your Life For Today", 555, feb)})
	h.searchResult["Live Your Life"] = []spotifyTrack{
		{ID: "sp-wrong", Artist: "Someone Else Entirely", Title: "Live Your Life For Today", Seconds: 553},
	}

	stats, err := h.Engine.RunOnce(ctx, false)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Added != 0 || len(h.Added) != 0 {
		t.Errorf("added %v; a wrong-artist match must not auto-add", h.Added)
	}
	if stats.Queued != 1 {
		t.Errorf("queued = %d, want 1", stats.Queued)
	}
	items := h.pending(t)
	if len(items) != 1 {
		t.Fatalf("review queue has %d items, want 1", len(items))
	}
	// The candidate and its rationale must survive for a human to judge.
	if len(items[0].Candidates) == 0 {
		t.Error("queued item carries no candidates to choose from")
	}
	if items[0].Candidates[0].Why == "" {
		t.Error("queued candidate carries no rationale")
	}
}

func TestRunOnce_NoSearchResultsRecordedAsNoMatch(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, []like{aLike(1, "Obscure Act", "Unreleased Dub", 400, feb)})
	// searchResult intentionally empty: Spotify returns nothing.

	stats, err := h.Engine.RunOnce(ctx, false)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Queued != 1 {
		t.Errorf("queued = %d, want 1", stats.Queued)
	}
	items := h.pending(t)
	if len(items) != 1 || items[0].Reason != sqlite.ReasonNoMatch {
		t.Fatalf("items = %+v, want one with reason %q", items, sqlite.ReasonNoMatch)
	}
}

// A track already on Spotify but absent from the ledger — a restored
// database, a `resync --forget-all`, or a manual add — must be recorded
// rather than added a second time.
func TestRunOnce_ReconcilesAgainstPlaylistInsteadOfDuplicating(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, []like{aLike(1, "DJ Rax", "Air Race (Spiritchaser Remix)", 480, feb)})
	h.searchResult["Air Race"] = []spotifyTrack{
		{ID: "sp1", Artist: "DJ Rax", Title: "Air Race - Spiritchaser Remix", Seconds: 480},
	}
	h.inPlaylist = []string{"sp1"}

	stats, err := h.Engine.RunOnce(ctx, false)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(h.Added) != 0 {
		t.Errorf("re-added %v to a playlist that already contains it", h.Added)
	}
	if stats.Added != 0 {
		t.Errorf("stats.Added = %d, want 0", stats.Added)
	}
	if n := h.ledgerCount(t); n != 1 {
		t.Errorf("ledger rows = %d, want 1 — the ledger should be repaired", n)
	}
}

func TestRunOnce_SkipsAlreadySyncedWithoutSearching(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, []like{aLike(1, "DJ Rax", "Air Race (Spiritchaser Remix)", 480, feb)})
	h.searchResult["Air Race"] = []spotifyTrack{
		{ID: "sp1", Artist: "DJ Rax", Title: "Air Race - Spiritchaser Remix", Seconds: 480},
	}

	if _, err := h.Engine.RunOnce(ctx, false); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	before := h.SearchCount

	// Reset the watermark so the like is fetched again; only the ledger
	// should now suppress it.
	if err := h.Store.ClearWatermark(ctx, h.Engine.Account.ID); err != nil {
		t.Fatalf("ClearWatermark: %v", err)
	}
	h.Engine.Account = h.reload(t)

	stats, err := h.Engine.RunOnce(ctx, false)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if stats.Added != 0 || len(h.Added) != 1 {
		t.Errorf("second pass added again: total adds %v", h.Added)
	}
	if h.SearchCount != before {
		t.Errorf("re-searched an already-synced track (%d -> %d)", before, h.SearchCount)
	}
}

// One failing search must not abort the batch — the remaining likes are
// still worth syncing — but it must not be mistaken for a verdict
// either. "We could not ask Spotify" is not "Spotify does not have it",
// and the watermark must stay put so the like is re-read next pass.
func TestRunOnce_SearchFailureDoesNotAbortPass(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, []like{
		aLike(1, "Broken Act", "Explodes", 400, feb),
		aLike(2, "DJ Rax", "Air Race (Spiritchaser Remix)", 480, mar),
	})
	h.failSearch["Explodes"] = true
	h.searchResult["Air Race"] = []spotifyTrack{
		{ID: "sp1", Artist: "DJ Rax", Title: "Air Race - Spiritchaser Remix", Seconds: 480},
	}

	// A swallowed failure is still reported to the caller, so a one-shot
	// run exits non-zero rather than looking like a clean pass.
	stats, err := h.Engine.RunOnce(ctx, false)
	if !errors.Is(err, syncer.ErrPassIncomplete) {
		t.Fatalf("RunOnce err = %v, want ErrPassIncomplete", err)
	}
	if stats.Added != 1 || len(h.Added) != 1 {
		t.Errorf("added = %v, want the healthy track to still sync", h.Added)
	}
	if stats.Queued != 0 {
		t.Errorf("queued = %d; a search *failure* must not be filed as a no-match verdict", stats.Queued)
	}
	if stats.Err == nil {
		t.Error("stats.Err is nil; the run log would show a clean pass")
	}
	// The whole point: the failed like must still be reachable.
	if got := h.reload(t).WatermarkLikedAt; !got.IsZero() {
		t.Errorf("watermark = %s, want unchanged — the pass was not clean", got)
	}
}

// A failed review-queue write is the other half of the same rule: the
// like ends up in neither the ledger nor the queue, so the watermark
// must not move past it. Otherwise it is gone with no trace anywhere.
func TestRunOnce_EnqueueFailureHoldsWatermark(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, []like{
		aLike(1, "Obscure Act", "Unfindable", 400, mar),
	})
	// No search result configured: this like would be queued as no_match.
	h.exec(t, `CREATE TRIGGER no_queue BEFORE INSERT ON review_queue
	           BEGIN SELECT RAISE(ABORT, 'simulated queue failure'); END;`)

	stats, err := h.Engine.RunOnce(ctx, false)
	if !errors.Is(err, syncer.ErrPassIncomplete) {
		t.Fatalf("RunOnce err = %v, want ErrPassIncomplete", err)
	}
	if stats.Err == nil {
		t.Error("stats.Err is nil; a swallowed enqueue failure would look like success")
	}
	if got := h.reload(t).WatermarkLikedAt; !got.IsZero() {
		t.Errorf("watermark = %s, want unchanged — the like was never durably recorded", got)
	}
	if items := h.pending(t); len(items) != 0 {
		t.Errorf("queue has %d items, expected the write to have failed", len(items))
	}
}

// Two DI.fm likes can resolve to one Spotify track — the same recording
// liked on two channels, or two DI.fm ids for one release. The playlist
// set is read once at the top of the pass, so without tracking adds made
// during the pass both copies get posted. The ledger does not catch it:
// it is keyed on the DI.fm track id, which differs.
func TestRunOnce_TwoLikesOneSpotifyTrackAddedOnce(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, []like{
		aLike(1, "DJ Rax", "Air Race (Spiritchaser Remix)", 480, feb),
		aLike(2, "DJ Rax", "Air Race (Spiritchaser Remix)", 480, mar),
	})
	h.searchResult["Air Race"] = []spotifyTrack{
		{ID: "sp1", Artist: "DJ Rax", Title: "Air Race - Spiritchaser Remix", Seconds: 480},
	}

	if _, err := h.Engine.RunOnce(ctx, false); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(h.Added) != 1 {
		t.Errorf("posted %v, want the track added exactly once", h.Added)
	}
}

// Invariant 3 exists because the ledger can be wrong — a restored
// database, a `resync --forget-all`, a manual add. Falling back to
// ledger-only dedupe when the reconciliation read fails degrades in
// exactly the situation the read exists to cover, and an add-only sync
// cannot undo the duplicate that follows.
func TestRunOnce_ReconcileFailureAbortsRatherThanDuplicating(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, []like{
		aLike(1, "DJ Rax", "Air Race (Spiritchaser Remix)", 480, mar),
	})
	h.searchResult["Air Race"] = []spotifyTrack{
		{ID: "sp1", Artist: "DJ Rax", Title: "Air Race - Spiritchaser Remix", Seconds: 480},
	}
	h.failPlaylistRead = true

	if _, err := h.Engine.RunOnce(ctx, false); err == nil {
		t.Fatal("expected the pass to fail when the playlist cannot be read")
	}
	if len(h.Added) != 0 {
		t.Errorf("posted %v without knowing the playlist contents", h.Added)
	}
	if got := h.reload(t).WatermarkLikedAt; !got.IsZero() {
		t.Errorf("watermark = %s, want unchanged", got)
	}
}

// A rate limit affects every remaining track, so the pass must stop
// rather than burn through the batch recording bogus verdicts.
func TestRunOnce_RateLimitAbortsPass(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, []like{
		aLike(1, "A", "One", 200, feb),
		aLike(2, "B", "Two", 200, mar),
	})
	h.rateLimitSearch = true

	_, err := h.Engine.RunOnce(ctx, false)
	if err == nil {
		t.Fatal("expected the pass to abort on a rate limit")
	}
	if !errors.Is(err, spotify.ErrRateLimited) {
		t.Errorf("err = %v, want it to carry ErrRateLimited", err)
	}
	if h.SearchCount != 1 {
		t.Errorf("issued %d searches, want 1 — a throttled API should not be hammered", h.SearchCount)
	}
	if got := h.reload(t).WatermarkLikedAt; !got.IsZero() {
		t.Errorf("watermark = %s, want unchanged", got)
	}
}

// `resync` is the recovery escape hatch, and the deployment keeps the
// daemon running, so it is always applied to a live engine. An engine
// holding a cached account ignores the reset and then writes the stale
// watermark back over it on the next tick.
func TestRunOnce_ResyncIsVisibleToARunningEngine(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, []like{
		aLike(1, "DJ Rax", "Air Race (Spiritchaser Remix)", 480, mar),
	})
	h.searchResult["Air Race"] = []spotifyTrack{
		{ID: "sp1", Artist: "DJ Rax", Title: "Air Race - Spiritchaser Remix", Seconds: 480},
	}

	if _, err := h.Engine.RunOnce(ctx, false); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if got := h.reload(t).WatermarkLikedAt; !got.Equal(mar) {
		t.Fatalf("watermark = %s, want %s after a clean pass", got, mar)
	}

	// An operator runs `resync --forget-all` against the same database
	// while this engine keeps running.
	if err := h.Store.ForgetAllTracks(ctx, h.Engine.Account.ID); err != nil {
		t.Fatalf("ForgetAllTracks: %v", err)
	}
	if err := h.Store.ClearWatermark(ctx, h.Engine.Account.ID); err != nil {
		t.Fatalf("ClearWatermark: %v", err)
	}

	stats, err := h.Engine.RunOnce(ctx, false)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if stats.Fetched != 1 {
		t.Errorf("fetched = %d, want 1 — the engine ignored the cleared watermark", stats.Fetched)
	}
	// It is already in the playlist, so reconciliation must record it
	// rather than add a duplicate.
	if len(h.Added) != 1 {
		t.Errorf("posted %v, want no re-add after resync", h.Added)
	}
}

// Shutdown mid-pass is the normal path for a deploy. The run row must
// still be closed out, or `difmsync status` shows a phantom in-flight
// run forever.
func TestRunOnce_CancellationStillClosesTheRunRow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newHarness(t, []like{
		aLike(1, "DJ Rax", "Air Race (Spiritchaser Remix)", 480, mar),
	})
	// Cancel partway in, as a SIGTERM during a deploy would.
	h.beforeSearch = cancel

	if _, err := h.Engine.RunOnce(ctx, false); err == nil {
		t.Fatal("expected a canceled pass to report an error")
	}

	runs, err := h.Store.ListRuns(context.Background(), h.Engine.Account.ID, 5)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("got %d runs, want 1", len(runs))
	}
	if runs[0].FinishedAt == "" {
		t.Error("run row left open after a canceled pass")
	}
	if got := h.reload(t).WatermarkLikedAt; !got.IsZero() {
		t.Errorf("watermark = %s, want unchanged on a canceled pass", got)
	}
}

func TestRunOnce_WatermarkAdvancesToNewestLike(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, []like{
		aLike(1, "DJ Rax", "Air Race (Spiritchaser Remix)", 480, feb),
		aLike(2, "DJ Rax", "Air Race (Spiritchaser Remix)", 480, mar),
	})
	h.searchResult["Air Race"] = []spotifyTrack{
		{ID: "sp1", Artist: "DJ Rax", Title: "Air Race - Spiritchaser Remix", Seconds: 480},
	}

	if _, err := h.Engine.RunOnce(ctx, false); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := h.reload(t).WatermarkLikedAt; !got.Equal(mar) {
		t.Errorf("watermark = %s, want the newest like %s", got, mar)
	}
}

func TestRunOnce_EmptyLikeListIsANoOp(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, nil)

	stats, err := h.Engine.RunOnce(ctx, false)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats != (sqlite.RunStats{}) {
		t.Errorf("stats = %+v, want zero", stats)
	}
	if len(h.Added) != 0 {
		t.Errorf("added %v with nothing to sync", h.Added)
	}
}

// TestRunOnce_LedgerNeverPrecedesTheSpotifyWrite pins invariant 1 on the
// path the in-pass dedupe fix created. Two DI.fm likes resolving to one
// Spotify track take different branches: the first queues an add, the
// second sees the track already "in playlist" — but only because the
// first marked it so, and the POST has not happened yet. Writing that
// second ledger row at match time put the ledger ahead of the Spotify
// write, so a crash in the window left a row claiming a track was synced
// that is not in the playlist and will never be re-added.
func TestRunOnce_LedgerNeverPrecedesTheSpotifyWrite(t *testing.T) {
	liked := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	h := newHarness(t, []like{
		{VoteID: 1, TrackID: 101, Artist: "Kiasmos", Title: "Blurred", Length: 300, LikedAt: liked},
		{VoteID: 2, TrackID: 102, Artist: "Kiasmos", Title: "Blurred", Length: 300, LikedAt: liked.Add(time.Minute)},
	})
	// Both likes resolve to the same Spotify recording, which is not yet
	// in the playlist.
	h.searchResult["Blurred"] = []spotifyTrack{{ID: "sp1", Artist: "Kiasmos", Title: "Blurred", Seconds: 300}}
	h.failAdd = true

	if _, err := h.Engine.RunOnce(context.Background(), false); err == nil {
		t.Fatal("RunOnce should have failed on the add")
	}

	if n := h.ledgerCount(t); n != 0 {
		t.Errorf("ledger rows = %d, want 0 — the add failed, so nothing is synced", n)
	}
	if got := h.reload(t).WatermarkLikedAt; !got.IsZero() {
		t.Errorf("watermark = %s, want zero — a failed add must not advance it", got)
	}
}

// TestRunOnce_TrackArrivingDuringThePassIsNotDuplicated closes the window
// between the reconciliation read and the add. `difmsync review --approve`
// writes to the same playlist from another process, and the pass's view of
// the playlist is N searches old by the time it posts. An add-only sync
// cannot remove the duplicate it would create.
func TestRunOnce_TrackArrivingDuringThePassIsNotDuplicated(t *testing.T) {
	liked := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	h := newHarness(t, []like{
		{VoteID: 1, TrackID: 101, Artist: "Kiasmos", Title: "Blurred", Length: 300, LikedAt: liked},
	})
	h.searchResult["Blurred"] = []spotifyTrack{{ID: "sp1", Artist: "Kiasmos", Title: "Blurred", Seconds: 300}}

	// After the pass's reconciliation read, someone else adds the track.
	h.afterPlaylistRead = func(reads int) {
		if reads == 1 {
			h.inPlaylist = append(h.inPlaylist, "sp1")
		}
	}

	if _, err := h.Engine.RunOnce(context.Background(), false); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(h.Added) != 0 {
		t.Errorf("posted %v, want nothing — the track was already there by the time we wrote", h.Added)
	}
	// It is still ours to record: the like is satisfied, so the ledger
	// row and the watermark must both land or the next pass re-searches it.
	if n := h.ledgerCount(t); n != 1 {
		t.Errorf("ledger rows = %d, want 1", n)
	}
	if got := h.reload(t).WatermarkLikedAt; !got.Equal(liked) {
		t.Errorf("watermark = %s, want %s", got, liked)
	}
}

// TestRunOnce_UnreadableLikeHoldsTheWatermark: a record the DI.fm client
// could not decode is skipped so the batch survives, but it is a like that
// reached no durable state anywhere. Advancing over it means nothing ever
// re-reads it — the watermark filters at fetch time — and `resync` cannot
// recover what was never recorded.
func TestRunOnce_UnreadableLikeHoldsTheWatermark(t *testing.T) {
	liked := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	h := newHarness(t, []like{
		{VoteID: 1, TrackID: 101, Artist: "Kiasmos", Title: "Blurred", Length: 300, LikedAt: liked},
	})
	h.searchResult["Blurred"] = []spotifyTrack{{ID: "sp1", Artist: "Kiasmos", Title: "Blurred", Seconds: 300}}
	// length arrives as a string: the record decodes to nothing usable.
	h.rawRecords = []string{`{"id":9,"track_id":109,"up":true,"created_at":"2026-03-02T12:00:00Z",
		"track":{"id":109,"title":"Drifted","display_artist":"X","length":"300","mix":false}}`}

	// The pass is reported as incomplete so a one-shot run exits non-zero.
	if _, err := h.Engine.RunOnce(context.Background(), false); !errors.Is(err, syncer.ErrPassIncomplete) {
		t.Fatalf("RunOnce err = %v, want ErrPassIncomplete", err)
	}

	// The readable like still syncs — one bad record costs one like.
	if len(h.Added) != 1 {
		t.Errorf("added %v, want the one readable like", h.Added)
	}
	// But the pass is not clean, so the watermark stays put.
	if got := h.reload(t).WatermarkLikedAt; !got.IsZero() {
		t.Errorf("watermark = %s, want zero — a dropped record makes the pass unclean", got)
	}
}

// TestLoopClampsANonPositiveInterval: rand.Int64N panics on a
// non-positive argument, so a zero or negative --interval would panic
// during Loop's startup — crashing the container on boot, before any
// log line explains why. There was no Loop test at all.
func TestLoopClampsANonPositiveInterval(t *testing.T) {
	for _, interval := range []time.Duration{0, -5 * time.Second, time.Millisecond} {
		t.Run(interval.String(), func(t *testing.T) {
			h := newHarness(t, nil)

			// Cancel immediately: the assertion is that Loop reaches its
			// select without panicking, not that it completes a pass.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			done := make(chan error, 1)
			go func() {
				defer func() {
					if r := recover(); r != nil {
						done <- fmt.Errorf("Loop panicked on interval %s: %v", interval, r)
					}
				}()
				done <- h.Engine.Loop(ctx, interval, true)
			}()

			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Loop did not return after its context was canceled")
			}
		})
	}
}
