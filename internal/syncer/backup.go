package syncer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// snapshotPrefix and snapshotSuffix bracket a scheduled snapshot's name.
// The date between them is ISO, so a lexical sort is a chronological
// one — which is what lets the prune below read a directory rather than
// stat every file in it.
const (
	snapshotPrefix = "difmsync-"
	snapshotSuffix = ".db"
	snapshotDay    = "2006-01-02"
)

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
}

// run takes at most one snapshot per UTC day and prunes older ones.
//
// "At most one" is decided from the directory rather than from stored
// state: today's name either exists or it does not, which survives a
// restart and needs no column. The cost is that deleting today's file by
// hand makes the next pass take another — which is the behavior an
// operator deleting a backup would expect anyway.
func (b *Backups) run(ctx context.Context, store snapshotter, label string) (string, error) {
	day := time.Now().UTC().Format(snapshotDay)
	dest := filepath.Join(b.Dir, snapshotPrefix+day+snapshotSuffix)
	if _, err := os.Stat(dest); err == nil {
		return "", nil // today's is already taken
	}
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

// prune deletes all but the newest Keep snapshots.
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
		if strings.HasPrefix(e.Name(), snapshotPrefix) && strings.HasSuffix(e.Name(), snapshotSuffix) {
			names = append(names, e.Name())
		}
	}
	if len(names) <= b.Keep {
		return nil
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names))) // ISO dates: lexical is chronological
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
