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
	"os"
	"sort"
	"strings"
	"time"

	"github.com/mjrossi/difm-spotify-sync/internal/store/sqlite"
)

// snapshotPrefix and snapshotSuffix bracket a scheduled snapshot's name,
// exactly as internal/syncer/backup.go names them (difmsync-YYYY-MM-DD.db).
//
// Duplicated rather than imported. internal/syncer's own test file already
// imports internal/status (TestKeepRunsIsTheHealthScanWindow), so an
// import the other way would be a cycle that breaks both packages' tests.
// Exporting the constants from syncer and importing them here would still
// point the dependency the wrong way — status is the read-only reporting
// package and must not know about the engine that writes what it reads.
// Three lines of prefix/suffix matching is cheaper than that coupling.
const (
	snapshotPrefix = "difmsync-"
	snapshotSuffix = ".db"
	snapshotDay    = "2006-01-02"
)

// snapshotDate reports the date encoded in a scheduled snapshot's name,
// and whether name actually has that shape. Matching the affixes is not
// enough: an operator's `difmsync backup --to=difmsync-before-upgrade.db`,
// or a half-copied file, matches difmsync-*.db without being a date, and
// "zzz" sorts lexically above every real ISO date — so before this
// existed, such a name could be read as "the newest backup" and published
// verbatim as last_backup_at on an unauthenticated LAN endpoint. Kept in
// sync with internal/syncer's own snapshotDate by
// TestLastBackupAtIgnoresNamesThatAreNotDates rather than by import, for
// the same cycle reason as the constants above.
func snapshotDate(name string) (string, bool) {
	if !strings.HasPrefix(name, snapshotPrefix) || !strings.HasSuffix(name, snapshotSuffix) {
		return "", false
	}
	mid := strings.TrimSuffix(strings.TrimPrefix(name, snapshotPrefix), snapshotSuffix)
	if _, err := time.Parse(snapshotDay, mid); err != nil {
		return "", false
	}
	return mid, true
}

// DefaultRunLimit is how many sync_runs rows a report carries when the
// caller does not ask for a specific number.
const DefaultRunLimit = 5

// HealthScanLimit is how far back the health verdict — and the failure
// count alongside it — look, independent of how many rows the caller
// wants reported. Exported so main can name it in what it prints,
// rather than repeating the number as a literal.
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
const HealthScanLimit = 20

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
	// Version is what the answering binary calls itself. A probe that
	// sees a stale healthy report wants to know which build produced it.
	Version string `json:"version"`
	// LastSuccessAt is the finished_at of the run the health rule
	// accepted — the same row, never a second query that could disagree.
	// Absent when no clean pass exists in the window, including when the
	// last one has fallen out of it.
	LastSuccessAt string `json:"last_success_at,omitempty"`
	// ConsecutiveFailures counts finished, non-dry, errored runs newer
	// than that row, within the scan window. In-flight rows are skipped.
	// At most HealthScanLimit — the scan window — so 20 means at least 20.
	ConsecutiveFailures int `json:"consecutive_failures"`
	// LastBackupAt is the newest scheduled snapshot's date (YYYY-MM-DD),
	// read straight from the backup directory listing — never from a
	// store column, so there is no second place for it to disagree with
	// what internal/syncer's Backups actually wrote. Empty when no
	// backup directory is configured, it has no snapshots yet, or it
	// could not be read; none of those states affect Healthy.
	LastBackupAt string `json:"last_backup_at,omitempty"`
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
	// ErrorKind is the one thing the endpoints may say about a failed
	// pass: a category the engine chose from its own sentinels, never
	// text an API returned. Empty for a clean pass or a pre-1.1 row.
	ErrorKind string `json:"error_kind,omitempty"`
	Error     string `json:"-"`
}

// newRun copies one store row into the reported view, field by field.
func newRun(r sqlite.SyncRun) Run {
	// The write side already refuses to record a kind it did not define
	// (FinishRun), but this endpoint answers the LAN from whatever
	// database it was handed — a restored or hand-edited one included —
	// so the exclusion is repeated here rather than trusted across
	// processes. See TestStatusJSONDropsAnUnknownKind.
	var kind string
	if r.ErrorKind.Known() {
		kind = string(r.ErrorKind)
	}
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
		ErrorKind:  kind,
		Error:      r.Error,
	}
}

// Build assembles a Report. maxAge is how stale the newest clean pass may
// be before the account is reported unhealthy; runLimit caps the number of
// sync_runs rows *reported* (<= 0 means DefaultRunLimit).
//
// runLimit deliberately does not affect Healthy. The health scan always
// covers HealthScanLimit rows and the list is truncated afterwards, so
// two callers asking for different amounts of detail cannot disagree
// about whether the sync is working.
func Build(
	ctx context.Context,
	store *sqlite.Store,
	label string,
	maxAge time.Duration,
	runLimit int,
	version string,
	backupDir string,
) (Report, error) {
	if runLimit <= 0 {
		runLimit = DefaultRunLimit
	}

	account, counts, runs, err := read(ctx, store, label, max(runLimit, HealthScanLimit))
	if err != nil {
		return Report{}, err
	}

	// The verdict and the count use the fixed window regardless of how
	// many rows the caller asked to see — in either direction. Narrowing
	// was closed once (TestHealthIgnoresRunLimit); widening was not.
	window := runs[:min(len(runs), HealthScanLimit)]
	healthy, reason, accepted := health(window, maxAge, time.Now())

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

	// Computed over the same fixed window, before runs is truncated to
	// what the caller asked to see — same reasoning as health() above.
	failures := consecutiveFailures(window, accepted)

	if len(runs) > runLimit {
		runs = runs[:runLimit]
	}
	return assemble(account, counts, runs, authorized, healthy, reason, version,
		accepted, failures, lastBackupAt(backupDir)), nil
}

// lastBackupAt reports the newest scheduled snapshot's date, or "" when
// backupDir is unset, has no matching file, or cannot be read.
//
// A probe that fails because the backup directory is missing or
// unreadable is worse than one that just reports no backup yet — a
// misconfigured or not-yet-created backup directory must not turn into a
// 500 on /status.json — so every error here is swallowed rather than
// returned. The health verdict is computed entirely from sync_runs and
// never touches this function, so a missing backup can never be read as
// "syncing stopped".
func lastBackupAt(backupDir string) string {
	if backupDir == "" {
		return ""
	}
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		return ""
	}
	var days []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if day, ok := snapshotDate(e.Name()); ok {
			days = append(days, day)
		}
	}
	if len(days) == 0 {
		return ""
	}
	// Validated ISO dates: lexical order is chronological, so the
	// greatest one is the newest snapshot.
	sort.Strings(days)
	return days[len(days)-1]
}

// counts holds the three totals the report carries, read together because
// they are always read together.
type counts struct {
	synced     int64
	pending    int64
	actionable int64
}

// read gathers everything Build reports on. Split out so Build reads as the
// three decisions it makes rather than as five sequential store calls with
// the decisions buried among them.
func read(ctx context.Context, store *sqlite.Store, label string, scan int) (
	sqlite.Account, counts, []sqlite.SyncRun, error,
) {
	account, err := store.GetAccount(ctx, label)
	if err != nil {
		return sqlite.Account{}, counts{}, nil, fmt.Errorf(
			"no account %q yet — run `difmsync auth` first: %w", label, err)
	}

	var c counts
	if c.synced, err = store.CountSynced(ctx, account.ID); err != nil {
		return sqlite.Account{}, counts{}, nil, err
	}
	// COUNT(*), not len() of a capped listing: a queue past the cap
	// previously reported the cap as its size.
	if c.pending, err = store.CountReview(ctx, account.ID, "pending"); err != nil {
		return sqlite.Account{}, counts{}, nil, err
	}
	if c.actionable, err = store.CountActionableReview(ctx, account.ID); err != nil {
		return sqlite.Account{}, counts{}, nil, err
	}

	runs, err := store.ListRuns(ctx, account.ID, scan)
	if err != nil {
		return sqlite.Account{}, counts{}, nil, err
	}
	return account, c, runs, nil
}

// assemble builds the report field by field from typed values.
//
// Never by serializing a store struct, and that is a rule rather than a
// description: accounts carries the Spotify refresh token on the same row as
// the label and the watermark, and structural exclusion is the only thing
// keeping it out of the JSON. Runs was briefly []sqlite.SyncRun, which
// published sync_runs.error — carrying the DI.fm member id, since *url.Error
// embeds the request URL. TestReportCarriesNoSecrets is what keeps this true.
func assemble(
	account sqlite.Account,
	c counts,
	runs []sqlite.SyncRun,
	authorized, healthy bool,
	reason string,
	version string,
	accepted *sqlite.SyncRun,
	failures int,
	lastBackupAt string,
) Report {
	// make, not a nil slice: an account with no runs should encode as
	// "runs": [] rather than "runs": null.
	reported := make([]Run, 0, len(runs))
	for _, run := range runs {
		reported = append(reported, newRun(run))
	}

	r := Report{
		Account:             account.Label,
		Playlist:            account.SpotifyPlaylistID,
		Authorized:          authorized,
		Synced:              c.synced,
		Pending:             c.actionable,
		Skipped:             c.pending - c.actionable,
		Runs:                reported,
		Healthy:             healthy,
		Reason:              reason,
		Version:             version,
		ConsecutiveFailures: failures,
		LastBackupAt:        lastBackupAt,
	}
	if accepted != nil {
		r.LastSuccessAt = accepted.FinishedAt
	}
	if !account.WatermarkLikedAt.IsZero() {
		r.Watermark = account.WatermarkLikedAt.UTC().Format(time.RFC3339)
	}
	return r
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
// that matters. The caller passes the full HealthScanLimit window, never
// a caller-chosen display slice — see Build.
func health(runs []sqlite.SyncRun, maxAge time.Duration, now time.Time) (healthy bool, reason string, accepted *sqlite.SyncRun) {
	if len(runs) == 0 {
		return false, "no sync pass has run yet", nil
	}
	for i := range runs {
		run := &runs[i]
		if run.DryRun || run.FinishedAt == "" || run.Error != "" {
			continue
		}
		at, err := time.Parse(sqlite.TimeFormat, run.FinishedAt)
		if err != nil {
			// Not fatal on its own — keep looking for a row we can read
			// rather than reporting unhealthy over a formatting problem.
			continue
		}
		// The stale case still returns the row: it is still the last
		// success, just too old — so last_success_at is reported
		// alongside the unhealthy reason.
		if age := now.Sub(at); age > maxAge {
			return false, fmt.Sprintf("last clean pass finished %s ago (max %s)",
				age.Round(time.Second), maxAge), run
		}
		return true, "", run
	}
	// Something ran, but nothing that counts. Say which, because "no clean
	// pass" and "no pass at all" call for different first moves.
	return false, fmt.Sprintf("no clean pass in the last %d run(s): %s",
		len(runs), describe(runs[0])), nil
}

// consecutiveFailures counts the finished, non-dry, errored runs newer
// than the accepted row (or all of them in the window when there is
// none). In-flight rows are skipped, not counted: a pass that is running
// has not failed yet.
//
// The clause set (DryRun, FinishedAt == "", Error == "") is deliberately
// identical to health()'s, so this count is the complement of the
// verdict rather than a second definition of "failed" that could drift
// from it. FinishedAt == "" stays even though this package never writes
// a row with Error set and FinishedAt empty: it does not trust the
// writer, and a restored or hand-edited database can still hand it a
// row that breaks that pairing — a row is still just a row here.
func consecutiveFailures(runs []sqlite.SyncRun, accepted *sqlite.SyncRun) int {
	n := 0
	for i := range runs {
		run := &runs[i]
		if accepted != nil && run.ID == accepted.ID {
			break
		}
		if run.DryRun || run.FinishedAt == "" || run.Error == "" {
			continue
		}
		n++
	}
	return n
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
//
// A reason may name the recorded *kind* — an enum this code defined —
// and nothing else from the row.
func describe(run sqlite.SyncRun) string {
	// The kind is checked first because it is the only thing about a
	// failed run this function may say. It is an enum the engine chose
	// (sqlite.RunErrorKind), so naming it here is not interpolation.
	switch run.ErrorKind {
	case sqlite.KindSpotifyGrantRevoked:
		return "newest run found the Spotify grant revoked; if consent has not been re-given, open the consent URL from the daemon log or run difmsync auth"
	case sqlite.KindDiFMUnauthorized:
		return "newest run had its DI.fm API key rejected — set a fresh DIFMSYNC_API_KEY; the README Credentials section says where to find it"
	case sqlite.KindRateLimited:
		return "newest run was rate limited by an API; the daemon backs off before retrying"
	}
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
