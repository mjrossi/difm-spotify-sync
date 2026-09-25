// Scheduling: when the next pass runs and what a pass failure means.

package syncer

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/mjrossi/difm-spotify-sync/internal/store/sqlite"
	"github.com/mjrossi/difm-spotify-sync/pkg/difm"
	"github.com/mjrossi/difm-spotify-sync/pkg/spotify"
)

// MinInterval floors the sync interval. Below this the jitter
// computation degenerates and the API traffic stops being polite.
const MinInterval = time.Minute

// RunsRetention is how long sync_runs history is kept. Not a flag: a
// retention period is not a knob a self-hoster needs, and every flag
// is a README row the config-drift test then polices. Ninety days at
// the default 15m interval is under nine thousand rows.
const RunsRetention = 90 * 24 * time.Hour

// KeepRuns is the floor pruning never goes below, whatever the age. It
// is the health scan window: the rule reads that many rows and must
// never lose one to housekeeping. TestKeepRunsIsTheHealthScanWindow
// pins the two together: the engine must not depend on the reporting
// layer that reads the store downstream of it, so the two are pinned
// by a test rather than by a shared constant.
const KeepRuns = 20

// classify maps the error a pass ended with onto the kind recorded in
// sync_runs. It lives here rather than in the store because the store
// must not import the API packages, and here rather than at each return
// site because a kind set in one branch and forgotten in another is the
// failure mode. RunOnce still sets KindIncomplete itself at the sites
// that return ErrPassIncomplete, because its FinishRun defer sees the
// first swallowed error rather than the wrapper — but the wrapper is
// what Loop receives, so it is classified here too and the log line and
// the row agree.
func classify(err error) sqlite.RunErrorKind {
	switch {
	case errors.Is(err, ErrPassIncomplete):
		return sqlite.KindIncomplete
	case errors.Is(err, spotify.ErrGrantRevoked):
		return sqlite.KindSpotifyGrantRevoked
	case errors.Is(err, difm.ErrUnauthorized):
		return sqlite.KindDiFMUnauthorized
	case errors.Is(err, spotify.ErrRateLimited), errors.Is(err, difm.ErrRateLimited):
		return sqlite.KindRateLimited
	default:
		return sqlite.KindError
	}
}

// maxRetryDelay caps how long a Retry-After may push the next pass.
// Spotify's can genuinely be hours for a Development Mode app, and
// honoring that is right; a header past a day is treated as a mistake
// rather than an instruction to park the daemon.
const maxRetryDelay = 24 * time.Hour

// nextDelay decides when the next pass runs, given how the last one
// ended. A rate limit from either API carries the server's own backoff
// hint; ignoring it and waiting a full interval turns one 429 at a long
// interval into hours of nothing, and a hint longer than the interval
// is honored because retrying sooner only guarantees another 429. The
// hint is clamped to the same floor Loop applies to the interval — a
// 429 aborts the pass, so the retry is a full pass start including a
// DI.fm fetch, and the floor is what keeps a tiny hint from driving
// that at more than one a minute — and to maxRetryDelay above.
// Anything else, including a 429 with no header, keeps the interval.
func nextDelay(err error, interval time.Duration) time.Duration {
	var retryAfter time.Duration
	var sp *spotify.RateLimitError
	var dr *difm.RateLimitError
	switch {
	case errors.As(err, &sp):
		retryAfter = sp.RetryAfter
	case errors.As(err, &dr):
		retryAfter = dr.RetryAfter
	}
	if retryAfter <= 0 {
		return interval
	}
	return min(max(retryAfter, MinInterval), maxRetryDelay)
}

// Loop runs passes on an interval until ctx is canceled, or until a pass
// reports that Spotify revoked the grant. The first tick is jittered so
// multiple deployments don't stampede the APIs together.
//
// A revoked grant is the one pass failure Loop does not ride out. The
// engine cannot fix it — consent is a cmd/difmsync concern — and ticking
// on would fail identically every interval while the row that could
// re-open consent sat unread. Returning spotify.ErrGrantRevoked hands the
// decision to the caller, which clears the token and re-enters the
// consent wait.
func (e *Engine) Loop(ctx context.Context, interval time.Duration, dryRun bool) error {
	// The interval is operator-supplied via --interval/DIFMSYNC_INTERVAL.
	// Two separate reasons to floor it: rand.Int64N panics outright below
	// 4ns, and anything under a minute stops being polite to a private
	// API. The floor is set by the second, which is why a deliberate
	// --interval=30s is overridden rather than honored — it warns.
	if interval < MinInterval {
		e.Log.Warn("interval too small; clamping",
			"requested", interval, "using", MinInterval)
		interval = MinInterval
	}
	after := e.after
	if after == nil {
		after = time.After
	}
	now := e.now
	if now == nil {
		now = time.Now
	}
	jitter := time.Duration(rand.Int64N(int64(interval / 4)))
	e.Log.Info("starting sync loop", "interval", interval, "first_run_in", jitter,
		"next_run", now().Add(jitter).Format(time.RFC3339))

	// A wait abandoned on return is not stopped. That is fine on both
	// return paths: context cancellation is process exit, and the
	// revoked-grant return happens right after a pass, before a new wait
	// is armed. Go 1.23+ collects an unreferenced timer anyway.
	wait := after(jitter)
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
		case <-wait:
			stats, err := e.RunOnce(ctx, dryRun)
			if errors.Is(err, spotify.ErrGrantRevoked) {
				return err
			}
			if err != nil {
				// Keep looping: a transient API failure should not kill
				// a long-running daemon. ErrPassIncomplete in particular
				// is self-correcting — the watermark held, so the next
				// tick re-reads whatever was missed.
				e.Log.Error("sync pass failed", "err", err)
			}
			delay := nextDelay(err, interval)
			if delay != interval {
				e.Log.Warn("rate limited; delaying next pass", "delay", delay, "interval", interval)
			}
			// The one line an idle pass leaves at Info. It carries the
			// counts and the next attempt; the error text, when there is
			// one, is on the "sync pass failed" line above and not here.
			// A revoked grant returns above without it: there is no next
			// run to name.
			attrs := []any{
				"fetched", stats.Fetched, "added", stats.Added,
				"queued", stats.Queued, "skipped", stats.Skipped,
				"clean", err == nil,
				"dry_run", dryRun,
			}
			if err != nil {
				// The same enum /status.json publishes as error_kind, so
				// a log line and the endpoint always agree on why a pass
				// failed — never the error text, which stays on the
				// "sync pass failed" line above.
				attrs = append(attrs, "kind", classify(err))
			}
			attrs = append(attrs, "next_run", now().Add(delay).Format(time.RFC3339))
			e.Log.Info("pass finished", attrs...)
			wait = after(delay)
		}
	}
}
