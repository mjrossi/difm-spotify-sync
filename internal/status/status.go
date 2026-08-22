// Package status builds the operator's view of the service: ledger
// totals, the review backlog, the watermark, and the recent sync_runs
// rows — plus a single verdict on whether syncing is actually happening.
//
// It exists so the CLI (`difmsync status`), the container healthcheck
// (`difmsync status --check`) and the HTTP endpoints cannot disagree.
// Health that is computed in two places drifts, and the direction it
// drifts is always the same: the probe keeps reporting green after the
// thing it probes has stopped working.
package status

import (
	"context"
	"fmt"
	"time"

	"github.com/mjrossi/difm-spotify-sync/internal/store/sqlite"
)

// DefaultRunLimit is how many sync_runs rows a report carries when the
// caller does not ask for a specific number.
const DefaultRunLimit = 5

// healthScanLimit is how far back the health verdict looks, independent of
// how many rows the caller wants reported.
//
// These have to be separate numbers. health() scans for the newest row
// that *qualifies* — finished, no error, not a dry run — so a caller
// asking for a short list was silently also narrowing the search. At a
// limit of 1 that was reliably wrong: the engine opens a sync_runs row
// when a pass starts and closes it when it ends, so for the whole
// duration of every pass the only visible row was the in-flight one and
// the verdict flipped to unhealthy. A window this size also absorbs a
// run of dry runs or failures without losing sight of the clean pass
// behind them.
const healthScanLimit = 20

// Report is the whole operator-visible state of one account.
//
// Every field is populated explicitly from a typed store accessor. That
// is deliberate and load-bearing: it is what guarantees the Spotify
// refresh token — which lives on the same accounts row as Label and
// WatermarkLikedAt — cannot reach the JSON encoder by accident. See
// TestReportCarriesNoSecrets.
type Report struct {
	Account   string `json:"account"`
	Playlist  string `json:"playlist"`
	Synced    int64  `json:"synced"`
	Pending   int64  `json:"pending"`
	Skipped   int64  `json:"skipped"`
	Watermark string `json:"watermark"`
	Runs      []Run  `json:"runs"`
	// Authorized reports whether the one-time Spotify consent has been
	// completed. A bool derived from the refresh token, never the token —
	// the field-by-field rule above is what keeps that distinction, and
	// TestReportCarriesNoSecrets is what keeps it true.
	Authorized bool   `json:"authorized"`
	Healthy    bool   `json:"healthy"`
	Reason     string `json:"reason,omitempty"`
}

// Run is one recorded pass as the operator surface reports it.
//
// This exists rather than serving sqlite.SyncRun directly because the
// field-by-field rule has to cover the whole payload, not just the
// accounts row. Serving a store struct means every column added to
// sync_runs later is published the moment it is added, with nothing in
// the way to catch it — which is exactly how Error got out.
//
// Error is json:"-" on purpose, and that is the structural half of the
// fix rather than a formatting choice. The engine records failures as
// err.Error() text, and that text is assembled from wherever the failure
// came from — DI.fm request URLs, Spotify responses, file paths. None of
// it is reviewed before it lands, so none of it can be published to an
// endpoint that is served to the LAN unauthenticated. Failed carries the
// one bit a JSON consumer actually needs; the text stays available to
// the CLI, which prints it in the runs table for an operator who already
// has the database.
type Run struct {
	ID         int64  `json:"id"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
	DryRun     bool   `json:"dry_run"`
	Fetched    int    `json:"fetched"`
	Added      int    `json:"added"`
	Queued     int    `json:"queued"`
	Skipped    int    `json:"skipped"`
	Failed     bool   `json:"failed"`
	Error      string `json:"-"`
}

// newRun copies one store row into the reported view, field by field.
func newRun(r sqlite.SyncRun) Run {
	return Run{
		ID:         r.ID,
		StartedAt:  r.StartedAt,
		FinishedAt: r.FinishedAt,
		DryRun:     r.DryRun,
		Fetched:    r.Fetched,
		Added:      r.Added,
		Queued:     r.Queued,
		Skipped:    r.Skipped,
		Failed:     r.Error != "",
		Error:      r.Error,
	}
}

// Build assembles a Report. maxAge is how stale the newest clean pass may
// be before the account is reported unhealthy; runLimit caps the number of
// sync_runs rows *reported* (<= 0 means DefaultRunLimit).
//
// runLimit deliberately does not affect Healthy. The health scan always
// covers healthScanLimit rows and the list is truncated afterwards, so
// two callers asking for different amounts of detail cannot disagree
// about whether the sync is working.
func Build(
	ctx context.Context,
	store *sqlite.Store,
	label string,
	maxAge time.Duration,
	runLimit int,
) (Report, error) {
	if runLimit <= 0 {
		runLimit = DefaultRunLimit
	}

	account, err := store.GetAccount(ctx, label)
	if err != nil {
		return Report{}, fmt.Errorf("no account %q yet — run `difmsync auth` first: %w", label, err)
	}

	synced, err := store.CountSynced(ctx, account.ID)
	if err != nil {
		return Report{}, err
	}
	// COUNT(*), not len() of a capped listing: a queue past the cap
	// previously reported the cap as its size.
	pending, err := store.CountReview(ctx, account.ID, "pending")
	if err != nil {
		return Report{}, err
	}
	actionable, err := store.CountActionableReview(ctx, account.ID)
	if err != nil {
		return Report{}, err
	}
	scan := max(runLimit, healthScanLimit)
	runs, err := store.ListRuns(ctx, account.ID, scan)
	if err != nil {
		return Report{}, err
	}
	// Decide first, over the whole scan window, then trim to what the
	// caller asked to see.
	healthy, reason := health(runs, maxAge, time.Now())
	// Checked after health() rather than inside it, because health() is
	// about whether passes are completing and this is about whether the
	// daemon has been given the credentials to run one at all. It wins
	// when both apply: "no sync pass has run yet" is true of a freshly
	// deployed container, but it sends an operator to the logs looking
	// for a failure when what is actually needed is one click.
	authorized := account.SpotifyRefreshToken != ""
	if !authorized {
		healthy = false
		reason = "awaiting Spotify consent — open the authorization URL from the daemon log"
	}
	if len(runs) > runLimit {
		runs = runs[:runLimit]
	}
	// make, not a nil slice: an account with no runs should encode as
	// "runs": [] rather than "runs": null.
	reported := make([]Run, 0, len(runs))
	for _, run := range runs {
		reported = append(reported, newRun(run))
	}

	r := Report{
		Account:    account.Label,
		Playlist:   account.SpotifyPlaylistID,
		Authorized: authorized,
		Synced:     synced,
		Pending:    actionable,
		Skipped:    pending - actionable,
		Runs:       reported,
	}
	if !account.WatermarkLikedAt.IsZero() {
		r.Watermark = account.WatermarkLikedAt.UTC().Format(time.RFC3339)
	}
	r.Healthy, r.Reason = healthy, reason
	return r, nil
}

// health decides whether syncing is actually happening, and says why not
// when it is not.
//
// The rule is "the newest pass that finished, wrote no error, and was not
// a dry run is no older than maxAge". Each clause earns its place:
//
//   - Unfinished rows are in-flight passes, or passes killed mid-run. An
//     open row is not evidence of success.
//   - A non-empty error column is a pass that swallowed something. The
//     engine holds the watermark back for exactly these, so such a row is
//     not itself evidence of success. Note this disqualifies the *row*,
//     not the account: the loop below keeps scanning, so an errored newest
//     run still reports healthy when a clean pass behind it is inside
//     maxAge. That is intended — the sync is demonstrably working — and
//     TestHealth pins it.
//   - Dry runs are excluded because the deployed loop never dry-runs. A
//     stale `just dry-run` from a debugging session would otherwise keep
//     the probe green over a daemon that has not completed a real pass in
//     days — the precise failure this function exists to catch.
//
// ListRuns orders newest-first, so the first qualifying row is the one
// that matters. The caller passes the full healthScanLimit window, never
// a caller-chosen display slice — see Build.
func health(runs []sqlite.SyncRun, maxAge time.Duration, now time.Time) (bool, string) {
	if len(runs) == 0 {
		return false, "no sync pass has run yet"
	}
	for _, run := range runs {
		if run.DryRun || run.FinishedAt == "" || run.Error != "" {
			continue
		}
		at, err := time.Parse(sqlite.TimeFormat, run.FinishedAt)
		if err != nil {
			// Not fatal on its own — keep looking for a row we can read
			// rather than reporting unhealthy over a formatting problem.
			continue
		}
		if age := now.Sub(at); age > maxAge {
			return false, fmt.Sprintf("last clean pass finished %s ago (max %s)",
				age.Round(time.Second), maxAge)
		}
		return true, ""
	}
	// Something ran, but nothing that counts. Say which, because "no clean
	// pass" and "no pass at all" call for different first moves.
	return false, fmt.Sprintf("no clean pass in the last %d run(s): %s",
		len(runs), describe(runs[0]))
}

// describe summarizes why one run did not count, for the unhealthy reason
// string. The newest run is the most useful one to name.
//
// The recorded error text is deliberately *not* interpolated here, even
// though it is the most informative thing available. Reason is written
// verbatim by /healthz on the 503 path — as plain text, to an endpoint
// served unauthenticated on the LAN — so anything this returns is
// published. Recorded error text is assembled from whatever failed and is
// reviewed by nobody; the Run.Error comment above has the longer version.
// Naming the run and pointing at the CLI keeps the reason actionable
// without turning the probe into a disclosure channel.
func describe(run sqlite.SyncRun) string {
	switch {
	case run.Error != "":
		return "newest run errored — run `difmsync status` for the error text"
	case run.FinishedAt == "":
		return "newest run is still in flight or was killed mid-pass"
	case run.DryRun:
		return "newest run was a dry run"
	default:
		return "newest run has an unreadable timestamp"
	}
}
