package syncer

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// snapshotPrefix and snapshotSuffix bracket a scheduled snapshot's name.
// The date between them is ISO, so a lexical sort over the *validated*
// names is a chronological one — which is what lets the prune below read
// a directory rather than stat every file in it.
//
// "Between the affixes" is a shape this code writes, not one it can
// assume every file in the directory has. A name like
// difmsync-before-upgrade.db (a manual `backup --to=`) or a half-copied
// file matches the prefix and suffix without being a date, and "zzz"
// sorts lexically above every real ISO date — so trusting the affixes
// alone let one such file get treated as the newest snapshot and delete
// every real one out from under it. snapshotDate parses the middle and
// is the only thing that may call a name a snapshot.
const (
	snapshotPrefix = "difmsync-"
	snapshotSuffix = ".db"
	snapshotDay    = "2006-01-02"
)

// snapshotDate reports whether name has the shape this code writes
// (difmsync-YYYY-MM-DD.db) and, if so, the date in the middle. A name
// that merely matches the prefix and suffix but is not a parseable date
// is not a snapshot this code owns, and prune must leave it alone.
func snapshotDate(name string) (time.Time, bool) {
	if !strings.HasPrefix(name, snapshotPrefix) || !strings.HasSuffix(name, snapshotSuffix) {
		return time.Time{}, false
	}
	mid := strings.TrimSuffix(strings.TrimPrefix(name, snapshotPrefix), snapshotSuffix)
	t, err := time.Parse(snapshotDay, mid)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// Backups configures the daemon's own scheduled snapshot. Zero value
// (a nil *Backups) takes none.
//
// It exists because the runbook's answer — a host cron running `docker
// compose exec` — runs as root, so every snapshot it writes and the
// directory it creates are root-owned, which is the failure CLAUDE.md's
// Deployment section says lasts longest. Taken by the daemon, they are
// owned by whoever the daemon already runs as.
type Backups struct {
	// Dir holds the snapshots. Empty means take none.
	Dir string
	// Keep is how many to retain, newest first. Zero or less keeps all.
	Keep int

	// lastAttempt is the UTC day (YYYY-MM-DD) run last attempted a
	// snapshot, set whether that attempt succeeded or failed. It exists
	// so a broken destination — a full volume, an unwritable directory —
	// is retried once a day rather than once a pass: the old gate was
	// "today's file exists", which is true only on success, so a
	// permanent failure re-attempted (and re-warned) on every single
	// pass, up to 96 times a day at the default interval.
	//
	// Unguarded because a *Backups is owned by exactly one Engine and
	// run is only ever called from that Engine's own RunOnce, one
	// pass at a time — never shared across Engines or called
	// concurrently with itself. A *Backups handed to more than one
	// caller would need a mutex around this field.
	//
	// Reset on restart, which is the right bias rather than a gap: a
	// transient cause (a volume that has since been cleared, a directory
	// that now exists) should not need surviving a restart to be retried
	// — and remembering "attempted" durably would mean a column and a
	// migration for a value that is only ever advisory.
	lastAttempt string
}

// take makes one backup attempt through run and logs what happened.
//
// A snapshot that landed is reported as written even when the prune
// after it failed, and the prune failure gets a Warn of its own. Folding
// both into "could not take a backup" told the operator a backup was
// missing when it was sitting on disk, and hid that the real problem was
// the directory filling up.
func (b *Backups) take(ctx context.Context, store snapshotter, label string, log *slog.Logger) {
	dest, err := b.run(ctx, store, label)
	if dest != "" {
		log.Info("backup written", "path", dest)
	}
	switch {
	case err == nil:
	case dest != "":
		log.Warn("could not prune old backups", "dir", b.Dir, "err", err)
	default:
		log.Warn("could not take a backup", "dir", b.Dir, "err", err)
	}
}

// run takes at most one snapshot attempt per UTC day and prunes older
// snapshots after a successful one.
//
// "At most one attempt" — not "at most one success" — is the point:
// lastAttempt is set before the write is tried, so a failure counts
// exactly the same as a success for the purpose of not trying again
// until tomorrow. That is also what keeps the caller's Warn log to one
// line per day instead of one per pass: with the attempt gated, the
// failure that produces it can only happen once a day.
//
// Restart resets lastAttempt (see its comment), and the file check below
// survives that: if a snapshot already landed before a restart, that
// check still catches it and takes none. Deleting today's file by hand
// makes the next pass take another — which is the behavior an operator
// deleting a backup would expect.
func (b *Backups) run(ctx context.Context, store snapshotter, label string) (string, error) {
	day := time.Now().UTC().Format(snapshotDay)
	if b.lastAttempt == day {
		return "", nil // already attempted today, win or lose
	}
	dest := filepath.Join(b.Dir, snapshotPrefix+day+snapshotSuffix)
	if _, err := os.Stat(dest); err == nil {
		return "", nil // today's is already taken
	}
	// Set before the attempt, not after: every return below this line —
	// success or failure — must count as today's attempt.
	b.lastAttempt = day
	if err := os.MkdirAll(b.Dir, 0o700); err != nil {
		return "", fmt.Errorf("creating the backup directory: %w", err)
	}
	if err := store.SnapshotTo(ctx, dest, label); err != nil {
		return "", err
	}
	if err := b.prune(); err != nil {
		// The snapshot itself landed; say so even though the tidy-up
		// failed, because the snapshot is the part that matters.
		return dest, fmt.Errorf("pruning old snapshots: %w", err)
	}
	return dest, nil
}

// prune deletes all but the newest Keep snapshots — snapshots meaning
// only names snapshotDate can parse. A file that merely matches the
// prefix and suffix without being a date is not one this code took, and
// is left alone rather than treated as some indeterminate age.
func (b *Backups) prune() error {
	if b.Keep <= 0 {
		return nil
	}
	entries, err := os.ReadDir(b.Dir)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if _, ok := snapshotDate(e.Name()); ok {
			names = append(names, e.Name())
		}
	}
	if len(names) <= b.Keep {
		return nil
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names))) // validated ISO dates: lexical is chronological
	for _, name := range names[b.Keep:] {
		if err := os.Remove(filepath.Join(b.Dir, name)); err != nil {
			return err
		}
	}
	return nil
}

// snapshotter is the one store method Backups needs, named here so the
// engine's dependency on the store stays legible.
type snapshotter interface {
	SnapshotTo(ctx context.Context, dest, verifyLabel string) error
}
