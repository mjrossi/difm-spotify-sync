package syncer

import (
	"errors"
	"time"

	"github.com/mjrossi/difm-spotify-sync/internal/store/sqlite"
	"github.com/mjrossi/difm-spotify-sync/pkg/difm"
	"github.com/mjrossi/difm-spotify-sync/pkg/spotify"
)

// classify maps the error a pass ended with onto the kind recorded in
// sync_runs. It lives here rather than in the store because the store
// must not import the API packages, and here rather than at each return
// site because a kind set in one branch and forgotten in another is the
// failure mode. KindIncomplete is the one kind not decided here: RunOnce
// sets it at the single site that returns ErrPassIncomplete.
func classify(err error) sqlite.RunErrorKind {
	switch {
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
	return min(max(retryAfter, minInterval), maxRetryDelay)
}
