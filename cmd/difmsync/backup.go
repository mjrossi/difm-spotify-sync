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
// the deployed database lives inside a container built on distroless:
// there is no shell and no sqlite3 binary in there, so the recipe's
// `sqlite3 ".backup"` cannot reach it. `docker compose exec connector
// /app/difmsync backup --to=...` can.
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
			store, err := openStore(ctx, c)
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

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
			if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
				return fmt.Errorf("creating the backup directory: %w", err)
			}
			if err := store.BackupTo(ctx, dest); err != nil {
				return err
			}
			// The snapshot inherits the process umask, and it contains the
			// refresh token — which grants unattended playlist write access
			// until revoked. Narrow it before anything else can read it.
			if err := os.Chmod(dest, 0o600); err != nil {
				return fmt.Errorf("restricting permissions on %s: %w", dest, err)
			}

			// Verify by reopening the snapshot and reading the account out
			// of it. Reopening rather than trusting the write is the point:
			// it proves the result is a database that opens and holds the
			// row that matters, not just bytes that landed.
			//
			// A backup without that row is not a backup, and saying so is
			// the whole reason for checking. Restoring one means writing it
			// *over* the live database, so a confident success message on
			// an empty file is the worst outcome available here. Removing
			// it is deliberate: a plausible-looking file left behind is how
			// it gets restored later by someone who never saw this error.
			label := c.String("account")
			if err := verifyBackup(ctx, dest, label); err != nil {
				if rmErr := os.Remove(dest); rmErr != nil {
					return fmt.Errorf("%w (and removing the unusable file failed: %w)", err, rmErr)
				}
				return fmt.Errorf("%w — removed it; check --db-path points at the database you meant", err)
			}

			fmt.Printf("backed up to %s (holds the Spotify refresh token — treat as a secret)\n", dest)
			return nil
		},
	}
}

// verifyBackup opens the snapshot and confirms it carries the account.
//
// Deliberately no Migrate: this must read what was written, not repair it
// into looking valid.
func verifyBackup(ctx context.Context, path, label string) error {
	store, err := sqlite.Open(path)
	if err != nil {
		return fmt.Errorf("backup at %s does not open as a database: %w", path, err)
	}
	defer func() { _ = store.Close() }()

	if _, err := store.GetAccount(ctx, label); err != nil {
		return fmt.Errorf("backup at %s has no %q account row: %w", path, label, err)
	}
	return nil
}
