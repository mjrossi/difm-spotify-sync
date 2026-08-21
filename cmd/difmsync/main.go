// Command difmsync mirrors DI.fm liked tracks into a Spotify playlist.
//
// Subcommands:
//
//	auth    one-time Spotify OAuth consent; stores the refresh token
//	sync    one pass (--dry-run) or a continuous loop (--loop)
//	review  inspect and resolve the review queue
//	status  recent sync runs and ledger totals; --check is the healthcheck
//	backup  consistent snapshot of the database
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/mjrossi/difm-spotify-sync/internal/status"
	"github.com/mjrossi/difm-spotify-sync/internal/store/sqlite"
	"github.com/mjrossi/difm-spotify-sync/internal/syncer"
	"github.com/mjrossi/difm-spotify-sync/pkg/difm"
	"github.com/mjrossi/difm-spotify-sync/pkg/spotify"
)

const defaultAccountLabel = "default"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	// Signal-aware root context: SIGINT/SIGTERM cancel mid-pass, which
	// the engine treats as a clean stop with the watermark unadvanced.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return newApp().Run(ctx, os.Args)
}

// newApp builds the command tree. Separated from run so tests can drive
// the real flag parsing, env-var fallbacks and subcommand wiring rather
// than reaching past them.
func newApp() *cli.Command {
	cmd := &cli.Command{
		Name:  "difmsync",
		Usage: "sync DI.fm liked tracks into a Spotify playlist",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name: "db-path", Usage: "SQLite database path",
				Value: "./tmp/difmsync.db", Sources: cli.EnvVars("DIFMSYNC_DB_PATH"),
			},
			&cli.StringFlag{
				Name: "log-format", Usage: "log output format: json|text",
				Value: "json", Sources: cli.EnvVars("DIFMSYNC_LOG_FORMAT"),
			},
			&cli.StringFlag{
				Name: "log-level", Usage: "log level: debug|info|warn|error",
				Value: "info", Sources: cli.EnvVars("DIFMSYNC_LOG_LEVEL"),
			},
			&cli.StringFlag{
				Name: "account", Usage: "account label (multi-account seam)",
				Value: defaultAccountLabel, Sources: cli.EnvVars("DIFMSYNC_ACCOUNT"),
			},
			&cli.StringFlag{
				Name: "api-key", Usage: "DI.fm API key (see docs/difm-api.md)",
				Sources: cli.EnvVars("DIFMSYNC_API_KEY"),
			},
			&cli.StringFlag{
				Name: "member-id", Usage: "DI.fm numeric member id",
				Sources: cli.EnvVars("DIFMSYNC_MEMBER_ID"),
			},
			&cli.StringFlag{
				Name: "network", Usage: "AudioAddict network slug: di|radiotunes|jazzradio|rockradio|classicalradio|zenradio",
				Value: difm.DefaultNetwork, Sources: cli.EnvVars("DIFMSYNC_NETWORK"),
			},
			&cli.StringFlag{
				Name: "playlist-id", Usage: "target Spotify playlist id",
				Sources: cli.EnvVars("DIFMSYNC_PLAYLIST_ID"),
			},
			&cli.StringFlag{
				Name: "spotify-client-id", Sources: cli.EnvVars("DIFMSYNC_SPOTIFY_CLIENT_ID"),
			},
			&cli.StringFlag{
				Name: "spotify-client-secret", Sources: cli.EnvVars("DIFMSYNC_SPOTIFY_CLIENT_SECRET"),
			},
			&cli.StringFlag{
				Name: "spotify-redirect-url", Value: "http://127.0.0.1:8888/callback",
				Sources: cli.EnvVars("DIFMSYNC_SPOTIFY_REDIRECT_URL"),
			},
			&cli.StringFlag{
				Name: "auth-bind",
				Usage: "host the `auth` listener binds to, overriding the redirect URL's host " +
					"(set to 0.0.0.0 in a container, where a published port forwards to eth0, not loopback)",
				Sources: cli.EnvVars("DIFMSYNC_AUTH_BIND"),
			},
			&cli.FloatFlag{
				Name: "auto-threshold", Usage: "score at or above which a match is added unasked",
				Value: 0.85, Sources: cli.EnvVars("DIFMSYNC_AUTO_THRESHOLD"),
			},
			&cli.FloatFlag{
				Name: "review-threshold", Usage: "score at or above which a match is queued for review",
				Value: 0.60, Sources: cli.EnvVars("DIFMSYNC_REVIEW_THRESHOLD"),
			},
		},
		Commands: []*cli.Command{
			syncCommand(),
			authCommand(),
			reviewCommand(),
			resyncCommand(),
			statusCommand(),
			backupCommand(),
		},
	}
	return cmd
}

func newLogger(c *cli.Command) *slog.Logger {
	var (
		level slog.Level
		bad   string
	)
	if err := level.UnmarshalText([]byte(c.String("log-level"))); err != nil {
		bad = c.String("log-level")
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}

	var log *slog.Logger
	if strings.EqualFold(c.String("log-format"), "text") {
		log = slog.New(slog.NewTextHandler(os.Stderr, opts))
	} else {
		log = slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	if bad != "" {
		// A typo'd DIFMSYNC_LOG_LEVEL that silently becomes "info" is the
		// kind of thing discovered while hunting for missing debug output.
		log.Warn("unrecognized log level; using info", "requested", bad)
	}
	return log
}

// openStore opens the database, creating the parent directory if needed,
// and applies migrations. Every subcommand starts here.
// mountDiagnostic turns SQLite's "unable to open database file (14)" —
// which names neither the path nor the reason — into something an
// operator can act on.
//
// This is the first thing a container deployment hits when the data
// volume is owned by root and the process is not. The image stages /data
// with --chown, which covers a Docker *named* volume: Docker seeds a
// fresh named volume from the image's directory, ownership included.
//
// It does not cover a bind mount. Pointing /data at a host directory —
// the obvious move for putting the database on a NAS or in a snapshotted
// dataset — mounts that directory with the ownership it already has, and
// nothing in the image can change it. See docs/deploy.md.
func mountDiagnostic(path string) string {
	dir := filepath.Dir(path)
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Sprintf("\n  (could not stat %s: %v)", dir, err)
	}
	var owner string
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		owner = fmt.Sprintf(", owned by uid %d gid %d", st.Uid, st.Gid)
	}
	return fmt.Sprintf("\n  %s has mode %s%s; this process runs as uid %d gid %d."+
		"\n  If those disagree, the mounted volume is not writable by the service —"+
		"\n  see the volume ownership section of docs/deploy.md.",
		dir, fi.Mode().Perm(), owner, os.Getuid(), os.Getgid())
}

func openStore(ctx context.Context, c *cli.Command) (*sqlite.Store, error) {
	path := c.String("db-path")
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create db directory %s: %w", dir, err)
		}
	}
	// The database holds the Spotify refresh token, which grants
	// unattended playlist write access until it is revoked, and the
	// driver would otherwise create the file at the process umask —
	// typically 0644. Created here, before Open, so the file never
	// exists world-readable at all; chmod after the fact leaves a window,
	// however brief, and the token is the one asset worth closing it for.
	// A path carrying a driver query string is passed through untouched:
	// pre-creating it would make a file literally named "x.db?_txlock=...".
	if path != ":memory:" && !strings.Contains(path, "?") {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, fmt.Errorf("create %s: %w%s", path, err, mountDiagnostic(path))
		}
		_ = f.Close()
		// Existing databases predate this and are still 0644.
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, fmt.Errorf("restrict permissions on %s: %w", path, err)
		}
	}

	store, err := sqlite.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%w%s", err, mountDiagnostic(path))
	}
	store.SetLogger(newLogger(c))

	if err := store.Migrate(ctx); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

func requireFlags(c *cli.Command, names ...string) error {
	var missing []string
	for _, n := range names {
		if strings.TrimSpace(c.String(n)) == "" {
			missing = append(missing, "--"+n)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required configuration: %s", strings.Join(missing, ", "))
	}
	return nil
}

func syncCommand() *cli.Command {
	return &cli.Command{
		Name:  "sync",
		Usage: "run a sync pass",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "dry-run",
				Usage: "score everything and report, but write nothing to Spotify",
			},
			&cli.BoolFlag{
				Name:  "loop",
				Usage: "run continuously on --interval instead of exiting after one pass",
			},
			&cli.DurationFlag{
				Name: "interval", Value: 15 * time.Minute,
				Sources: cli.EnvVars("DIFMSYNC_INTERVAL"),
			},
			&cli.StringFlag{
				Name: "http-addr",
				Usage: "serve the read-only /healthz and /status.json endpoints on this " +
					"address (empty disables them; only applies with --loop)",
				Sources: cli.EnvVars("DIFMSYNC_HTTP_ADDR"),
			},
			&cli.DurationFlag{
				Name: "max-age", Value: 45 * time.Minute,
				Usage:   "how stale the last clean pass may be before /healthz reports unhealthy",
				Sources: cli.EnvVars("DIFMSYNC_STATUS_MAX_AGE"),
			},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if err := requireFlags(c, "api-key", "member-id", "playlist-id",
				"spotify-client-id", "spotify-client-secret"); err != nil {
				return err
			}
			log := newLogger(c)

			store, err := openStore(ctx, c)
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

			account, err := store.EnsureAccount(ctx, c.String("account"),
				c.String("member-id"), c.String("playlist-id"))
			if err != nil {
				return err
			}

			auth := spotify.NewAuthenticator(c.String("spotify-client-id"),
				c.String("spotify-client-secret"), c.String("spotify-redirect-url"))

			// Persist a rotated refresh token as Spotify issues it. Held
			// only in memory, a rotation survives until the next restart
			// and then leaves the daemon presenting a dead token — with
			// the interactive consent step as the only way back.
			sp, err := auth.Client(ctx, account.SpotifyRefreshToken, func(tok string) error {
				log.Info("spotify rotated the refresh token; persisting")
				return store.SetSpotifyRefreshToken(ctx, account.ID, tok)
			})
			if err != nil {
				return err
			}

			// Name the playlist in the log before writing to it, so a
			// misconfigured id is obvious rather than silently wrong.
			if name, err := sp.PlaylistName(ctx, account.SpotifyPlaylistID); err != nil {
				// Logged, not swallowed: this is the loudest early signal
				// that the deployment is pointed at the wrong playlist, or
				// that the grant lost its scopes. Discarding it defeats
				// the entire point of the check.
				log.Warn("could not read target playlist",
					"id", account.SpotifyPlaylistID, "err", err)
			} else {
				log.Info("target playlist", "id", account.SpotifyPlaylistID, "name", name)
			}

			difmClient := difm.New(c.String("api-key"), c.String("member-id"))
			difmClient.Network = c.String("network")
			difmClient.Logf = func(format string, args ...any) {
				log.Warn(fmt.Sprintf(format, args...))
			}

			engine := &syncer.Engine{
				DiFM:       difmClient,
				Spotify:    sp,
				Store:      store,
				Account:    account,
				PlaylistID: account.SpotifyPlaylistID,
				Thresholds: syncer.Thresholds{
					Auto:   c.Float("auto-threshold"),
					Review: c.Float("review-threshold"),
				},
				Log: log,
			}

			if !c.Bool("loop") {
				_, err = engine.RunOnce(ctx, c.Bool("dry-run"))
				return err
			}

			loop := func(ctx context.Context) error {
				return engine.Loop(ctx, c.Duration("interval"), c.Bool("dry-run"))
			}
			addr := c.String("http-addr")
			if addr == "" {
				return loop(ctx)
			}
			return serveWhile(ctx, addr,
				status.Handler(store, c.String("account"), c.Duration("max-age"), log),
				log, loop)
		},
	}
}

// serveWhile runs the read-only status endpoints for as long as work is
// running, and returns whichever of the two finishes first.
//
// Deliberately hand-rolled rather than reaching for errgroup:
// golang.org/x/sync is an indirect dependency today, and promoting it to
// a direct one for a WaitGroup with an error slot is not a trade this
// project makes (see CLAUDE.md on adding dependencies).
//
// The ordering matters. The listener is opened before work starts, so a
// bad --http-addr fails immediately and loudly instead of leaving a
// daemon that syncs fine but silently answers nothing — which reads as a
// dead service to whatever is polling it.
func serveWhile(
	ctx context.Context,
	addr string,
	h http.Handler,
	log *slog.Logger,
	work func(context.Context) error,
) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s for the status endpoints: %w", addr, err)
	}

	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()
	log.Info("status endpoints listening", "addr", ln.Addr().String(),
		"routes", "/healthz /status.json")

	defer func() {
		// A fresh context: ctx is already canceled on the shutdown path,
		// and passing a canceled one makes Shutdown return instantly
		// without draining anything.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Warn("status server shutdown", "err", err)
		}
	}()

	workErr := make(chan error, 1)
	go func() { workErr <- work(ctx) }()

	select {
	case err := <-serveErr:
		// The server died on its own. The sync loop may still be fine,
		// but it is now unobservable — and an unobservable daemon is the
		// exact failure this endpoint exists to prevent. Surface it and
		// let the restart policy start a whole one.
		if err != nil {
			return fmt.Errorf("status server: %w", err)
		}
		return nil
	case err := <-workErr:
		return err
	}
}

func reviewCommand() *cli.Command {
	return &cli.Command{
		Name:  "review",
		Usage: "list or resolve queued tracks that did not auto-add",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "status", Value: "pending", Usage: "pending|approved|rejected"},
			&cli.IntFlag{Name: "limit", Value: 50},
			&cli.IntFlag{Name: "approve", Usage: "approve a DI.fm track id: add its candidate to the playlist"},
			&cli.IntFlag{Name: "candidate", Value: 1, Usage: "which candidate to approve, 1-based (see --approve)"},
			&cli.IntFlag{Name: "reject", Usage: "mark a DI.fm track id rejected"},
			&cli.BoolFlag{Name: "json", Usage: "emit JSON instead of a table"},
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

			if id := c.Int("approve"); id != 0 {
				return approveReview(ctx, c, store, account, int64(id))
			}
			if id := c.Int("reject"); id != 0 {
				ok, err := store.ResolveReview(ctx, account.ID, int64(id), "rejected")
				if err != nil {
					return err
				}
				if !ok {
					return fmt.Errorf("no queued track with DI.fm id %d", id)
				}
				fmt.Printf("rejected %d\n", id)
				return nil
			}

			items, err := store.ListReview(ctx, account.ID, c.String("status"), c.Int("limit"))
			if err != nil {
				return err
			}
			if c.Bool("json") {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(items)
			}
			if len(items) == 0 {
				fmt.Println("review queue is empty")
				return nil
			}
			for _, it := range items {
				fmt.Printf("\n[%d] %s — %s\n", it.DifmTrackID, it.Artist, it.Title)
				fmt.Printf("    reason=%s best=%.3f duration=%ds\n", it.Reason, it.BestScore, it.DurationSec)
				for i, cand := range it.Candidates {
					fmt.Printf("      %d. %.3f  %s — %s  (%ds)  %s\n",
						i+1, cand.Score, cand.Artist, cand.Title, cand.DurationSec, cand.Why)
				}
				if it.DetailsURL != "" {
					fmt.Printf("    %s\n", it.DetailsURL)
				}
			}
			fmt.Printf("\n%d item(s). Resolve with --approve=<id> or --reject=<id>.\n", len(items))
			return nil
		},
	}
}

// approveReview adds a queued item's chosen candidate to the playlist,
// records it in the ledger, and marks the queue row approved.
//
// Approval used to flip the status column and stop there, which was a
// permanent no-op: the watermark has long since moved past the like, so
// nothing would ever revisit it and the track never reached Spotify.
func approveReview(ctx context.Context, c *cli.Command, store *sqlite.Store,
	account sqlite.Account, trackID int64,
) error {
	if err := requireFlags(c, "spotify-client-id", "spotify-client-secret"); err != nil {
		return fmt.Errorf("approving adds to Spotify, so credentials are needed: %w", err)
	}

	item, err := store.GetReviewItem(ctx, account.ID, trackID)
	if err != nil {
		return fmt.Errorf("no queued track with DI.fm id %d: %w", trackID, err)
	}
	if len(item.Candidates) == 0 {
		return fmt.Errorf("track %d has no candidates to approve (reason=%s); "+
			"nothing on Spotify was matched to it", trackID, item.Reason)
	}

	n := c.Int("candidate")
	if n < 1 || n > len(item.Candidates) {
		return fmt.Errorf("--candidate=%d out of range; track %d has %d candidate(s)",
			n, trackID, len(item.Candidates))
	}
	chosen := item.Candidates[n-1]

	log := newLogger(c)
	auth := spotify.NewAuthenticator(c.String("spotify-client-id"),
		c.String("spotify-client-secret"), c.String("spotify-redirect-url"))
	sp, err := auth.Client(ctx, account.SpotifyRefreshToken, func(tok string) error {
		return store.SetSpotifyRefreshToken(ctx, account.ID, tok)
	})
	if err != nil {
		return err
	}

	// Same reconciliation rule as a sync pass: check the live playlist
	// before writing, so approving something already present records it
	// rather than duplicating it.
	inPlaylist, err := sp.PlaylistTrackIDs(ctx, account.SpotifyPlaylistID)
	if err != nil {
		return fmt.Errorf("read playlist before approving: %w", err)
	}
	if !inPlaylist[chosen.ID] {
		if err := sp.AddToPlaylist(ctx, account.SpotifyPlaylistID, []string{chosen.ID}); err != nil {
			return err
		}
		log.Info("approved and added", "track", chosen.Artist+" - "+chosen.Title)
	} else {
		log.Info("approved; already in playlist", "track", chosen.Artist+" - "+chosen.Title)
	}

	// Ledger before queue status, matching the sync pass's ordering: a
	// crash here re-approves rather than losing the record of the add.
	if err := store.RecordSynced(ctx, sqlite.SyncedTrack{
		AccountID:      account.ID,
		DifmTrackID:    item.DifmTrackID,
		DifmVoteID:     item.DifmVoteID,
		SpotifyTrackID: chosen.ID,
		PlaylistID:     account.SpotifyPlaylistID,
		Artist:         item.Artist,
		Title:          item.Title,
		MatchScore:     chosen.Score,
		LikedAt:        item.LikedAt,
	}); err != nil {
		return err
	}
	// The bool is checked rather than discarded: this is the one caller
	// the return value was added for. Reaching here means GetReviewItem
	// found the row, so a miss now means it vanished mid-command — worth
	// saying out loud rather than reporting an approval that did nothing.
	found, err := store.ResolveReview(ctx, account.ID, trackID, "approved")
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("review row for track %d disappeared before it could be approved "+
			"(the track was added to the playlist and recorded in the ledger)", trackID)
	}
	fmt.Printf("approved %d -> %s — %s\n", trackID, chosen.Artist, chosen.Title)
	return nil
}

func statusCommand() *cli.Command {
	return &cli.Command{
		Name:  "status",
		Usage: "show ledger totals, the review backlog and the last sync runs",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "json", Usage: "emit JSON instead of a table"},
			&cli.BoolFlag{
				Name: "check",
				Usage: "exit non-zero unless a clean sync pass finished within --max-age " +
					"(this is the container healthcheck)",
			},
			&cli.IntFlag{Name: "limit", Value: status.DefaultRunLimit, Usage: "how many recent runs to show"},
			&cli.DurationFlag{
				Name: "max-age", Value: 45 * time.Minute,
				Usage:   "how stale the last clean pass may be before --check fails",
				Sources: cli.EnvVars("DIFMSYNC_STATUS_MAX_AGE"),
			},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			store, err := openStore(ctx, c)
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

			rep, err := status.Build(ctx, store, c.String("account"),
				c.Duration("max-age"), c.Int("limit"))
			if err != nil {
				return err
			}

			if c.Bool("check") {
				// One line, no report: this runs as the container
				// healthcheck, where the output lands in `docker inspect`
				// and nothing reads more than the first line of it.
				if !rep.Healthy {
					return errors.New(rep.Reason)
				}
				fmt.Println("ok")
				return nil
			}

			if c.Bool("json") {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(rep)
			}

			printStatus(rep)
			return nil
		},
	}
}

func printStatus(rep status.Report) {
	fmt.Printf("account:   %s\n", rep.Account)
	fmt.Printf("playlist:  %s\n", rep.Playlist)
	fmt.Printf("synced:    %d track(s)\n", rep.Synced)
	fmt.Printf("pending:   %d item(s) awaiting review", rep.Pending)
	if rep.Skipped > 0 {
		fmt.Printf(" (+%d skipped non-track(s) recorded)", rep.Skipped)
	}
	fmt.Println()
	if rep.Watermark == "" {
		fmt.Printf("watermark: none — next run reads full history\n")
	} else {
		fmt.Printf("watermark: %s\n", rep.Watermark)
	}
	if rep.Healthy {
		fmt.Printf("health:    ok\n")
	} else {
		fmt.Printf("health:    NOT OK — %s\n", rep.Reason)
	}

	// The runs table is the whole point of the command's usage string,
	// and until now it promised something it never printed. An empty
	// error column on the newest row is the "it worked" signal.
	fmt.Println()
	if len(rep.Runs) == 0 {
		fmt.Println("no sync runs recorded yet")
		return
	}
	fmt.Printf("%-20s  %-5s  %5s  %5s  %5s  %s\n",
		"STARTED", "DRY", "ADDED", "QUEUE", "SKIP", "ERROR")
	for _, run := range rep.Runs {
		dry := ""
		if run.DryRun {
			dry = "yes"
		}
		started := run.StartedAt
		if len(started) > 19 {
			started = started[:19]
		}
		fmt.Printf("%-20s  %-5s  %5d  %5d  %5d  %s\n",
			started, dry, run.Added, run.Queued, run.Skipped, run.Error)
	}
}
