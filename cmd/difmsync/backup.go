package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/urfave/cli/v3"

	"github.com/mjrossi/difm-spotify-sync/internal/store/sqlite"
)

// backupCommand takes a consistent snapshot of the database.
//
// This exists as a subcommand rather than only as a `just` recipe because
// the deployed database lives inside a container with no sqlite3 binary
// in it, so the recipe's `sqlite3 ".backup"` cannot reach it.
// `docker compose exec connector /difmsync backup --to=...` can.
//
// Through /difmsync rather than /app/difmsync: `docker exec` runs as
// root, and this command creates its destination directory. Run without
// the privilege drop it leaves /config/backups and every snapshot in it
// root-owned, which the daemon cannot then write to.
func backupCommand() *cli.Command {
	return &cli.Command{
		Name:  "backup",
		Usage: "write a consistent snapshot of the database (holds the Spotify refresh token)",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "to",
				Usage:    "destination path; must not already exist",
				Required: true,
			},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			return withStore(ctx, c, func(store *sqlite.Store) error {
				dest := c.String("to")
				// Checked before the write so the refusal reads as an
				// instruction rather than as SQLite's "SQL logic error:
				// output file already exists (1)". VACUUM INTO refuses an
				// existing path on its own; this only says so usefully.
				// Overwriting is not offered: the file in the way is often
				// the only copy of a refresh token.
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

				// Staged in a private directory and renamed into place, rather
				// than written straight to dest. Two reasons, both of which bit
				// the straightforward version:
				//
				//   - VACUUM INTO does not clean up after itself. A failure
				//     partway — a full volume is the realistic one, since the
				//     nightly backup writes to the same volume as the database —
				//     leaves its partial output behind. At the destination that
				//     is a truncated file with a plausible dated name, which is
				//     exactly what a later restore would copy over the live
				//     database.
				//   - The file is created with the process umask (0644 on a
				//     default setup) and can only be chmod'd once VACUUM
				//     returns, so the refresh token would be world-readable for
				//     however long the copy takes. MkdirTemp creates the staging
				//     directory 0700, which closes that window at the directory
				//     instead.
				//
				// Same parent as dest, so the rename stays on one filesystem.
				stage, err := os.MkdirTemp(parent, ".difmsync-backup-")
				if err != nil {
					return fmt.Errorf("creating a staging directory next to %s: %w", dest, err)
				}
				// Removes the staging directory and anything left in it on every
				// path out, so a failed backup leaves nothing behind at all.
				defer func() { _ = os.RemoveAll(stage) }()

				tmp := filepath.Join(stage, "difmsync.db")
				if err := store.BackupTo(ctx, tmp); err != nil {
					return err
				}
				if err := os.Chmod(tmp, 0o600); err != nil {
					return fmt.Errorf("restricting permissions on the snapshot: %w", err)
				}

				// Verify by reopening the snapshot and reading the account out
				// of it. Reopening rather than trusting the write is the point:
				// it proves the result is a database that opens and holds the
				// row that matters, not just bytes that landed.
				//
				// A backup without that row is not a backup, and saying so is
				// the whole reason for checking. Restoring one means writing it
				// *over* the live database, so a confident success message on an
				// empty file is the worst outcome available here. Verifying
				// before the rename means an unusable snapshot never reaches the
				// destination to be mistaken for a good one later.
				if err := verifyBackup(ctx, tmp, dest, c.String("account")); err != nil {
					return fmt.Errorf("%w — check --db-path points at the database you meant", err)
				}

				// Publish only what has been verified. The check at the top is
				// advisory rather than a lock — rename replaces — but what it
				// guards against is running the command twice by hand, and what
				// matters is the invariant that survives either way: nothing
				// unverified is ever written to dest.
				if err := os.Rename(tmp, dest); err != nil {
					return fmt.Errorf("moving the verified snapshot to %s: %w", dest, err)
				}

				fmt.Printf("backed up to %s (holds the Spotify refresh token — treat as a secret)\n", dest)
				return nil
			})
		},
	}
}

// verifyBackup opens the snapshot at path and confirms it carries the
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
func verifyBackup(ctx context.Context, path, dest, label string) error {
	store, err := sqlite.Open(path)
	if err != nil {
		return fmt.Errorf("the snapshot staged for %s does not open as a database: %w", dest, err)
	}
	defer func() { _ = store.Close() }()

	if _, err := store.GetAccount(ctx, label); err != nil {
		return fmt.Errorf("the snapshot staged for %s has no %q account row: %w", dest, label, err)
	}
	return nil
}
