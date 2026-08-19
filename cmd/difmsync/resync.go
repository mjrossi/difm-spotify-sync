package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/urfave/cli/v3"
)

// resyncCommand is the recovery escape hatch.
//
// Normal operation is one-way and add-only: tracks deleted from the
// Spotify playlist stay deleted, and un-liking on DI.fm does not remove
// anything. That is intentional. But it leaves no way back from an
// accidental deletion, because two independent mechanisms suppress a
// re-add — the ledger row, and the watermark.
//
// The watermark is the non-obvious half. It filters at fetch time, so
// clearing a ledger row alone is not enough: the like would never be
// retrieved to begin with. This command exists so both can be reset
// together, deliberately.
//
// Re-adding cannot duplicate: the sync pass reconciles against the live
// playlist contents before adding anything.
func resyncCommand() *cli.Command {
	return &cli.Command{
		Name:  "resync",
		Usage: "reset sync state so past likes are re-evaluated",
		Description: "Clears the watermark and, optionally, ledger rows, so the next `sync` " +
			"re-reads history. Use --forget to make specific tracks eligible to be re-added " +
			"after you deleted them from Spotify. Nothing is written to Spotify by this " +
			"command; run `sync` afterwards.",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "all",
				Usage: "clear the watermark so the next sync re-reads the full like history",
			},
			&cli.IntSliceFlag{
				Name:  "forget",
				Usage: "DI.fm track id to drop from the ledger (repeatable), making it re-addable",
			},
			&cli.BoolFlag{
				Name:  "forget-all",
				Usage: "drop every ledger row for this account (implies --all)",
			},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			store, err := openStore(ctx, c)
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

			account, err := store.GetAccount(ctx, c.String("account"))
			if err != nil {
				return fmt.Errorf("no account %q yet — run `difmsync sync` first: %w",
					c.String("account"), err)
			}

			forget := c.IntSlice("forget")
			forgetAll := c.Bool("forget-all")
			clearMark := c.Bool("all") || forgetAll

			if !clearMark && len(forget) == 0 {
				return errors.New("nothing to do: pass --all, --forget=<track-id>, or --forget-all")
			}

			switch {
			case forgetAll:
				if err := store.ForgetAllTracks(ctx, account.ID); err != nil {
					return err
				}
				fmt.Println("cleared the entire sync ledger")
			case len(forget) > 0:
				var missed int
				// The earliest like among the forgotten tracks. The
				// watermark is rewound to just before it, because
				// clearing the ledger row alone is a no-op once the
				// watermark has moved past the like — and by the time
				// anyone reaches for --forget, it has.
				var earliest time.Time
				for _, id := range forget {
					at, ok, err := store.SyncedTrackLikedAt(ctx, account.ID, int64(id))
					if err != nil {
						return err
					}
					if ok && (earliest.IsZero() || at.Before(earliest)) {
						earliest = at
					}
					found, err := store.ForgetTrack(ctx, account.ID, int64(id))
					if err != nil {
						return err
					}
					if found {
						fmt.Printf("forgot DI.fm track %d\n", id)
						continue
					}
					missed++
					fmt.Printf("no ledger row for DI.fm track %d — nothing to forget\n", id)
				}
				if missed == len(forget) && !clearMark {
					// Only bail when there is nothing else left to do. An
					// explicit --all is a separate instruction, and
					// returning here would silently drop it.
					return fmt.Errorf("none of the %d track id(s) were in the ledger; "+
						"check `difmsync status` or the synced_tracks table for the right ids", missed)
				}
				if missed == len(forget) {
					fmt.Printf("none of the %d track id(s) were in the ledger; "+
						"continuing because --all was given\n", missed)
				}

				// Rewind rather than clear: --all resets to the beginning
				// of history, which is a much bigger instruction than the
				// operator gave. One second before the like is the
				// smallest rewind that lets the next pass see it again.
				// Everything between there and now is already in the
				// ledger, so it is re-read cheaply and not re-added.
				if !clearMark && !earliest.IsZero() && !account.WatermarkLikedAt.Before(earliest) {
					rewind := earliest.Add(-time.Second)
					if err := store.SetWatermark(ctx, account.ID, rewind); err != nil {
						return err
					}
					fmt.Printf("rewound the watermark to %s so the forgotten like is re-read\n",
						rewind.Format(time.RFC3339))
				}
			}

			if clearMark {
				if err := store.ClearWatermark(ctx, account.ID); err != nil {
					return err
				}
				fmt.Println("cleared the watermark — next sync reads the full history")
			}

			fmt.Println("\nrun `difmsync sync --dry-run` to preview, then `difmsync sync`.")
			return nil
		},
	}
}
