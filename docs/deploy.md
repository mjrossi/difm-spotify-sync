# Deploy runbook

One container on a homelab host, on the internal network. The binary is
static (pure-Go SQLite driver, `CGO_ENABLED=0`) and the image is distroless
and non-root, so the whole deployment is that image plus a volume at
`/data`.

## Prerequisites

Five secrets. None may be committed.

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

### Day-2 commands

Use `exec`, not `run`. `exec` reaches into the container that is already
running, with the volume it already has; `run` starts another one and
invites the mistake above.

```sh
docker compose exec connector /app/difmsync status
docker compose exec connector /app/difmsync review
docker compose exec connector /app/difmsync review --approve=<difm-track-id>
docker compose exec connector /app/difmsync resync --forget=<id> --all
```

There is no shell in the image, so these are the whole interface — each
one is the binary invoked directly, not a command line for something to
interpret.

### Upgrading

```sh
git pull
docker compose build
docker compose up -d
```

Migrations are embedded and applied on every boot, so there is no separate
migration step. The build needs BuildKit — the Dockerfile uses
`RUN --mount=type=cache`, which the legacy builder rejects outright, so a
host with `DOCKER_BUILDKIT=0` fails immediately rather than subtly.

## Volume ownership

**Check this if the container crash-loops on first start.** The process
runs as uid 65532 (`nonroot`), and the image stages `/data` with
`--chown=65532:65532`. That is what a Docker *named* volume needs — Docker
seeds a fresh named volume from the image directory, ownership included.
The default `compose.yaml` uses a named volume, so this normally just
works.

It stops working the moment you switch `/data` to a **bind mount** — the
obvious move if you want the database on a NAS or inside a snapshotted
dataset:

```yaml
volumes:
  - /srv/difmsync:/data      # instead of difmsync-data:/data
```

A bind mount arrives with the ownership the host directory already has,
and nothing in the image can change it. If that directory is root-owned,
the service cannot create its database, and `restart: unless-stopped`
turns the failure into a crash loop rather than something you notice.

The binary reports this explicitly rather than leaving you with SQLite's
bare `unable to open database file (14)`:

```
error: sqlite.Open: ping: unable to open database file (14)
  /data has mode -rwxr-xr-x, owned by uid 0 gid 0; this process runs as uid 65532 gid 65532.
```

Check it before anything else goes wrong:

```sh
docker compose logs connector | head -30
```

If the uids disagree, chown the host directory once — from the host, since
the image deliberately has no shell:

```sh
sudo chown -R 65532:65532 /srv/difmsync
docker compose restart connector
```

Verify with the log line above rather than assuming it took.

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
just status      # the report: totals, watermark, health, recent runs
just review      # anything that didn't auto-add
```

`status` prints a runs table. An empty `ERROR` column on the newest row is
the signal that the last pass completed; a pass that fails still writes
its row, which is the point of the table.

**Idempotency check:** run `just sync` twice back to back. The second pass
must add zero tracks.

## Is it still working?

This is the question a homelab deployment actually needs answered, because
the sync interval is an internal ticker — nothing external triggers a
pass, so a container that stopped, wedged, or lost its credentials simply
stops syncing, quietly.

One rule answers it, and everything below uses that same rule: **the
newest pass that finished, recorded no error, and was not a dry run must
be within `DIFMSYNC_STATUS_MAX_AGE`** (45m by default — three ticks of the
15m interval, so one missed pass is tolerated and two are not).

```sh
docker compose ps                                       # healthy / unhealthy
docker compose exec connector /app/difmsync status --check   # the same verdict, with a reason
curl -s http://<host>:3436/healthz                      # 200 ok, or 503 and the reason
curl -s http://<host>:3436/status.json | jq             # the full report
```

The container healthcheck runs `status --check`, deliberately rather than
curling `/healthz`: distroless ships no curl or wget, and this way health
still works if `DIFMSYNC_HTTP_ADDR` is unset.

Point a dashboard (Uptime Kuma, Homepage, anything that polls a URL) at
`/healthz`. Both endpoints are read-only and carry no secrets, which is
what makes them safe to expose on the LAN without authentication.

### When it goes red

`/healthz` and `--check` both name the reason. Match it:

| Reason | What it means | First move |
|---|---|---|
| `no account "default" yet` | `difmsync auth` has never run against this volume | Run the auth step above |
| `no sync pass has run yet` | The container started but has not completed a pass | Wait one interval; then read the logs |
| `newest run errored: …` | A pass failed and the watermark was held back | The error is the whole message — read it |
| `last clean pass finished Nh ago` | Passes stopped completing | `docker compose logs --tail=100 connector` |
| `newest run is still in flight` | A pass is running, or was killed mid-run | Wait; if it persists, restart the container |

For the errored case, `/status.json` carries the last few runs with their
errors, which is usually faster than paging through logs:

```sh
curl -s http://<host>:3436/status.json | jq '.runs[] | {started_at, added, error}'
```

A failing pass is not data loss. The watermark only advances after a fully
clean pass, so whatever the failed pass missed is re-read on the next one.

## Backups

The database is the only copy of the Spotify refresh token, and losing it
means redoing the one interactive step in the whole system. It also holds
the ledger, the review queue and the watermark.

`difmsync backup` runs inside the container, which matters: the image is
distroless, so there is no `sqlite3` and no shell in there to copy the
file with.

```sh
docker compose exec connector /app/difmsync backup --to=/data/backups/difmsync-$(date +%F).db
```

`$(date)` expands in *your* shell, not the container's — which is what you
want, since the container has none.

That writes into the volume. Either let whatever backs up your Docker
volumes pick it up, or pull it onto the host:

```sh
docker compose cp connector:/data/backups/difmsync-$(date +%F).db ./difmsync-backup.db
```

As a nightly cron on the host:

```cron
15 4 * * * cd /srv/difm-spotify-sync && docker compose exec -T connector \
  /app/difmsync backup --to=/data/backups/difmsync-$(date +\%F).db
```

`-T` disables TTY allocation, which cron needs. Note the escaped `\%` —
cron treats a bare `%` as a newline. Prune old snapshots on whatever
schedule suits you; nothing rotates them.

Three things the command refuses to do, all for the same reason — the
output is often the only copy of a refresh token, and restoring one means
writing it *over* the live database:

- **Overwrite an existing destination.** Pick another `--to`, or move the
  old file away first.
- **Leave a snapshot it could not verify.** It reopens the result and
  reads the account row back. A file that fails is deleted, because a
  plausible-looking file left behind is how it gets restored later by
  someone who never saw the error.
- **Write it world-readable.** The snapshot is `chmod 600`.

### Restoring

```sh
docker compose stop connector
docker compose cp ./difmsync-backup.db connector:/data/difmsync.db
docker compose start connector
docker compose exec connector /app/difmsync status
```

Stop first: copying over a database with a live writer attached is how you
get a corrupt one. If `/data` is a bind mount, `chown 65532:65532` the
restored file — see [Volume ownership](#volume-ownership).

After a restore the ledger may be behind the playlist's real contents.
That is safe to fix: each pass reconciles against the live playlist before
adding, so `difmsync resync --forget-all` followed by a sync rebuilds the
ledger without duplicating anything.

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
