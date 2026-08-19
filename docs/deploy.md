# Deploy runbook

The same container image runs on a homelab host and on Fly.io. The binary is
static (pure-Go SQLite driver, `CGO_ENABLED=0`) and the image is distroless
and non-root, so nothing environment-specific is baked in — only the volume
mount and the secret source differ.

## Prerequisites

Both targets need five secrets. None may be committed.

| Variable | Where it comes from |
|---|---|
| `DIFMSYNC_API_KEY` | DI.fm page HTML — see [`difm-api.md`](difm-api.md) |
| `DIFMSYNC_MEMBER_ID` | Same capture |
| `DIFMSYNC_SPOTIFY_CLIENT_ID` | Spotify developer dashboard app |
| `DIFMSYNC_SPOTIFY_CLIENT_SECRET` | Same app |
| `DIFMSYNC_PLAYLIST_ID` | The target playlist's Spotify URL |

### Creating the Spotify app

1. <https://developer.spotify.com/dashboard> → **Create app**.
2. Add a redirect URI. It must match `DIFMSYNC_SPOTIFY_REDIRECT_URL`
   exactly; the default is `http://127.0.0.1:8888/callback`.
3. Copy the client ID and secret.

The playlist ID is the path segment in its URL:
`https://open.spotify.com/playlist/`**`37i9dQZF1DX...`**

## The auth step

Spotify's Authorization Code flow needs one interactive browser consent.
Everything after it is unattended — the refresh token is stored in the
SQLite database and the OAuth transport renews access tokens on its own.

This is the only step that cannot run headless, so **run it before
deploying**, and make sure the database it writes to is the one the
deployment will mount.

```sh
just auth        # prints a URL, waits on 127.0.0.1:8888 for the callback
```

The listener's address and path are both derived from
`DIFMSYNC_SPOTIFY_REDIRECT_URL`, so a custom redirect URI works as long
as it matches what you registered in the Spotify dashboard. Inside a
container the host has to be overridden — see below.

## Homelab (Docker Compose)

```sh
cp mise.local.toml.example .env   # then fill in; compose reads .env
docker compose run --rm --service-ports connector auth   # one-time
docker compose up -d
docker compose logs -f connector
```

Two things about that `auth` line, both easy to get wrong:

- **`--service-ports` is required.** `docker compose run` does not publish
  the service's ports without it, so the `ports:` block in `compose.yaml`
  has no effect on that command and the callback never arrives.
- **The listener must bind `0.0.0.0` inside the container.** A published
  port forwards to the container's eth0 address, not its loopback, so a
  listener on `127.0.0.1` is unreachable from the host. `compose.yaml`
  sets `DIFMSYNC_AUTH_BIND=0.0.0.0` for this. The redirect URL stays
  `http://127.0.0.1:8888/callback` — that is what your browser opens and
  what Spotify's dashboard requires.

The database lives on the `difmsync-data` named volume. To inspect it:

```sh
docker compose run --rm connector status
```

Do not add `-v difmsync-data:/data` here. Compose namespaces volumes by
project, so the real one is `<project>_difmsync-data`; naming the bare
`difmsync-data` creates a *second*, empty volume and mounts it over
`/data`, and `status` then reports no account at all — which reads as a
broken deployment rather than a mistyped command. The service already
declares the volume, so there is nothing to add.

## Fly.io

```sh
fly volumes create difmsync_data --size 1 --region iad

fly secrets set \
  DIFMSYNC_API_KEY=... \
  DIFMSYNC_MEMBER_ID=... \
  DIFMSYNC_SPOTIFY_CLIENT_ID=... \
  DIFMSYNC_SPOTIFY_CLIENT_SECRET=... \
  DIFMSYNC_PLAYLIST_ID=...

fly deploy

# Not optional — see "Do not let it scale to zero" below. Running the
# blocks above without this leaves a worker Fly is free to stop, and a
# stopped machine never syncs.
fly scale count 1
```

Two Fly-specific notes:

- **This is a worker, not a web service.** `fly.toml` has no
  `[http_service]` block and exposes no port. Fly's default health checks
  don't apply.
- **Do not let it scale to zero.** The sync interval is an internal ticker,
  so a stopped machine simply never syncs.

  `min_machines_running` and `auto_stop_machines` are **not** available to
  this app: they live inside `[http_service]` / `[[services]]`, and a
  worker has neither. What actually keeps it up is that there is no
  service for Fly's proxy to autostop, plus:

  ```sh
  fly scale count 1
  ```

  `fly.toml` also sets `[[restart]] policy = "always"`, so a worker that
  hits a transient failure at boot keeps being restarted rather than
  stopping for good after Fly's default retry budget.

### Getting the refresh token onto the volume

`difmsync auth` needs a browser, and the image has no shell for
`fly ssh console`. Run auth locally and move the whole database across —
do **not** read the token out and paste it around: it grants unattended
playlist write access until revoked, and echoing it puts it in your shell
history and terminal scrollback.

```sh
just auth                      # locally; writes ./tmp/difmsync.db
just backup ./tmp/upload.db    # consistent snapshot (a live WAL copy can tear)

fly ssh sftp put ./tmp/upload.db /data/difmsync.db
fly apps restart difm-spotify-sync
```

`fly ssh sftp` works without a shell in the image, which is why it is the
supported path here.

### Volume ownership

**Check this if the machine crash-loops on first deploy.** The process
runs as uid 65532 (`nonroot`), and the image stages `/data` with
`--chown=65532:65532`. That is what a Docker named volume needs — Docker
seeds a fresh volume from the image directory, ownership included.

A Fly volume is a freshly formatted block device, and a formatted
filesystem is mounted `root:root` no matter what the image contains. If
that is what your machine does, the service cannot create its database,
and `[[restart]] policy = "always"` turns it into a crash loop rather
than a visible failure. `fly ssh sftp put` also writes as **root**, so
even a successful upload can leave `/data/difmsync.db` unwritable by the
service.

The binary reports this explicitly rather than leaving you with SQLite's
bare `unable to open database file (14)`:

```
error: sqlite.Open: ping: unable to open database file (14)
  /data has mode -rwxr-xr-x, owned by uid 0 gid 0; this process runs as uid 65532 gid 65532.
```

Check it before anything else goes wrong:

```sh
fly logs -a difm-spotify-sync | head -30
```

If the uids disagree, the volume needs to be chowned to 65532 once, from
a context that has a shell — the image deliberately has none. The
straightforward route is a one-off machine on any image with a shell,
mounting the same volume:

```sh
fly machine run --volume difmsync_data:/data alpine \
  -a difm-spotify-sync -- chown -R 65532:65532 /data
```

Then destroy that machine and restart the app. Verify with the log line
above rather than assuming it took.

## First run

Always dry-run first. A one-way playlist append is tedious to undo by hand.

```sh
just dry-run     # scores everything, writes nothing, prints the full report
```

Read the report. Confirm the auto-add candidates are actually right, then:

```sh
just sync        # for real
just status      # ledger totals, pending reviews, watermark
```

Point `DIFMSYNC_PLAYLIST_ID` at a scratch playlist for the first live run.

## Verifying it works

```sh
just status                        # watermark should advance after a clean pass
just review                        # anything that didn't auto-add
sqlite3 "$DIFMSYNC_DB_PATH" \
  "select started_at, added, queued, skipped, error from sync_runs order by id desc limit 5;"
```

An empty `error` column on the most recent row is the signal that the last
pass completed. A pass that fails still writes its row — that is the point
of the table.

**Idempotency check:** run `just sync` twice back to back. The second pass
must add zero tracks.

## Backups

The database is the only copy of the Spotify refresh token, and losing it
means redoing the one interactive step in the whole system. Back it up:

```sh
just backup                        # -> ./tmp/difmsync-backup.db
just backup /path/to/somewhere.db
```

Use that rather than `cp`: the database runs in WAL mode, so copying the
file while the daemon is writing can capture a torn state. The backup
contains the refresh token — store it as a secret.

`just backup` reads `DIFMSYNC_DB_PATH`, defaulting to `./tmp/difmsync.db`
— **the local path, not the deployed one**. For the compose deployment
the database is inside the named volume, so copy it out first rather than
backing up a path that does not exist there:

```sh
docker compose cp connector:/data/difmsync.db ./tmp/difmsync.db
just backup
```

The recipe fails loudly if the source is missing or the result has no
`accounts` row. It has to: `sqlite3 ".backup"` on a nonexistent path
creates an empty database and exits 0, so without those checks a wrong
`DIFMSYNC_DB_PATH` yields a confident success message, a 4 KB file with
no tables, and — on the Fly path above, where that file is uploaded over
`/data/difmsync.db` — the loss of the only copy of the refresh token.

On Fly:

```sh
fly ssh sftp get /data/difmsync.db ./difmsync-backup.db
```

## Recovering a deleted track

The sync never re-adds what you deleted from Spotify. To override that:

```sh
sqlite3 "$DIFMSYNC_DB_PATH" 'select difm_track_id, artist, title from synced_tracks;'
difmsync resync --forget=<difm-track-id> --all
difmsync sync
```

`--all` is not optional in practice: it clears the watermark, without which
the like is never re-read regardless of the ledger.

To rebuild the ledger from scratch — after restoring a backup, say —
`difmsync resync --forget-all` then `difmsync sync`. No duplicates result;
the pass reconciles against the playlist's real contents first.

## Rotating the DI.fm key

There is no rotation UI — the key is not surfaced anywhere in DI.fm's
settings. If it stops working (`ErrUnauthorized` in the logs), re-capture it
per [`difm-api.md`](difm-api.md) and update the secret. Sync state in the
database is unaffected; the watermark picks up where it left off.
