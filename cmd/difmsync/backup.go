package main

import (
	"context"
	"fmt"

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
				if err := store.SnapshotTo(ctx, dest, c.String("account")); err != nil {
					return err
				}

				fmt.Printf("backed up to %s (holds the Spotify refresh token — treat as a secret)\n", dest)
				return nil
			})
		},
	}
}
