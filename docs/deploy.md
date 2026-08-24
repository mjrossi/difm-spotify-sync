# Deploy runbook

One container, one volume. The binary is static (pure-Go SQLite driver,
`CGO_ENABLED=0`), the image is Alpine-based and drops to an unprivileged
user at startup, and everything it keeps lives in the database mounted at
`/config`.

- [Prerequisites](#prerequisites)
- [Authorizing](#authorizing) — the one interactive step
- [Running it](#running-it)
- [Volume ownership](#volume-ownership) — read this if it crash-loops
- [Upgrading](#upgrading), including from the `/data` layout
- [Is it still working?](#is-it-still-working)
- [Backups](#backups) and [restoring](#restoring)

## Prerequisites

Five secrets. None may be committed.

| Variable | Where it comes from |
|---|---|
| `DIFMSYNC_API_KEY` | DI.fm page HTML — see [`difm-api.md`](difm-api.md) |
| `DIFMSYNC_MEMBER_ID` | Same capture |
| `DIFMSYNC_SPOTIFY_CLIENT_ID` | Spotify developer dashboard app |
| `DIFMSYNC_SPOTIFY_CLIENT_SECRET` | Same app |
| `DIFMSYNC_PLAYLIST_ID` | The target playlist's Spotify URL |

No DI.fm password is needed at any point.

### Creating the Spotify app

1. <https://developer.spotify.com/dashboard> → **Create app**.
2. Add the redirect URIs. Each must match `DIFMSYNC_SPOTIFY_REDIRECT_URL`
   exactly for the context that uses it, and the dashboard accepts more
   than one — register whichever of these you will use:
   - `http://127.0.0.1:3437/callback` — the image default. Used when you
     authorize from a browser on the Docker host, through an SSH tunnel,
     or with `auth --manual`.
   - `http://127.0.0.1:8888/callback` — the binary's own default, used by
     `just auth` on a workstation.
   - `https://<host>/difmsync/callback` — if you will authorize the
     deployed daemon through a proxy; see [Authorizing](#authorizing).
3. Copy the client ID and secret.

Spotify requires HTTPS for any redirect URI that is not a loopback
literal (`127.0.0.1` or `[::1]`), and rejects `localhost` outright. That
is why the loopback URLs above work with nothing in front of them, and
why reaching the daemon from anywhere else needs TLS — or the manual
flow, which needs neither.

The playlist ID is the path segment in its URL:
`https://open.spotify.com/playlist/`**`37i9dQZF1DX...`**

## Authorizing

Spotify's Authorization Code flow needs one interactive browser consent.
Everything after it is unattended: the refresh token is stored in the
database and the OAuth transport renews access tokens on its own.

While there is no refresh token, the daemon **waits** rather than exiting
— which under `restart: unless-stopped` was a crash loop rather than a
prompt. It serves the consent flow on `DIFMSYNC_AUTH_HTTP_ADDR` and logs
one line:

```
spotify consent required — open this URL to authorize url=http://127.0.0.1:3437/start?t=<nonce> listening=[::]:3437
```

Everything below is about getting a browser to that URL. Pick the first
one that describes your setup; they all write the same token to the same
place.

### The browser is on the Docker host

Open the URL. Nothing else to do.

### You can SSH to the host

```sh
ssh -L 3437:127.0.0.1:3437 <host>
```

Then open the logged URL on your own machine. This works with the
shipped defaults and needs no TLS: the tunnelled address is still
`127.0.0.1`, which is the loopback literal Spotify accepts over plain
HTTP. For most self-hosted setups this is the shortest path.

### Through Tailscale

`tailscale serve` gives you a real certificate with nothing exposed to
the internet:

```sh
tailscale serve --bg --set-path /difmsync 3437
tailscale serve status          # confirm the mapping
```

Then set, on that host:

```sh
DIFMSYNC_SPOTIFY_REDIRECT_URL=https://<node>.<tailnet>.ts.net/difmsync/callback
```

and register that same URL in the Spotify dashboard.

Note what this changes about the audience: with `tailscale serve` in
front, the consent port answers every peer on your tailnet rather than
one host. The nonce in the start URL is the guard that survives that —
see [the consent server's properties](#what-guards-the-consent-flow).

Whether a path-mounted proxy passes its prefix through to the backend or
strips it has changed across Tailscale releases. The daemon answers on
both the full path and its last segment for exactly that reason — a
mismatch would 404 the callback, which is indistinguishable from Spotify
never calling back. Anything that reaches neither is logged with the path
it asked for:

```sh
docker compose logs connector | grep unrouted
```

### Behind your own reverse proxy

Same shape as Tailscale. Proxy to port 3437, set
`DIFMSYNC_SPOTIFY_REDIRECT_URL` to the public HTTPS origin plus
`/difmsync/callback`, and register it. Both served paths are derived from
that URL, so scoping it to a subpath puts the start page at
`/difmsync/start` rather than claiming `/start` at the root of a hostname
that may front several services.

### None of the above: `auth --manual`

Nothing needs to be reachable. The redirect URI only has to be
*registered* with Spotify — you carry the callback back by hand.

```sh
docker exec -it difmsync /difmsync auth --manual
```

It prints the consent URL. Open it in any browser anywhere, approve, and
the browser lands on a page that will almost certainly fail to load —
that is expected, nothing is listening there. Copy the address out of the
address bar and paste it back:

```
Redirected URL: http://127.0.0.1:3437/callback?code=AQD...&state=7f3c...
Refresh token stored. `difmsync sync` can now run unattended.
```

Paste the **whole URL**, not just the code. The `state` beside it is the
CSRF guard, and the command refuses a bare code rather than working
around its absence.

A daemon already waiting for consent notices this within ten seconds and
starts syncing — it polls for a token rather than only watching its own
listener, so any of these routes ends the wait.

### On a workstation

```sh
just auth        # prints a URL, waits on 127.0.0.1:8888 for the callback
```

The listener's address and path are both derived from
`DIFMSYNC_SPOTIFY_REDIRECT_URL`. This path requires an `http` loopback
redirect URL and will refuse an `https` one — it serves plain HTTP and
cannot terminate TLS. Use `just auth-manual` if you need an https
redirect URI. **Make sure the database it writes to is the one the
deployment will mount.**

### What guards the consent flow

Two things, and they are different guards for different reasons.

The **nonce** in the start URL is generated at startup and emitted once,
to the log. Reaching the port is not enough to begin a flow — without
this, anyone who could reach it could complete consent with *their*
Spotify account and bind your sync to a stranger's playlist. The endpoint
is unauthenticated by necessity, since you have no session with it yet.

The **callback** is guarded by the OAuth `state` parameter instead,
because Spotify redirects a browser to it and will not carry an extra
parameter. That is the standard protection, and the same one `difmsync
auth` relies on.

The listener exists only while there is no refresh token and shuts down
for the life of the process once there is one. A *failed* consent
deliberately leaves it up — a denied grant or a mistyped state has to be
retryable by clicking the URL again, not by restarting the container.

## Running it

The README has the `docker run` and Compose snippets for the published
image. This repo's `compose.yaml` builds from source instead, which is
what you want if you are changing the code:

```sh
cp .env.local.example .env.local   # then fill in the five secrets
chmod 600 .env.local               # nothing does this for you
docker compose up -d
docker compose logs -f connector   # -> the consent URL, once
```

The first build prints `pull access denied for difm-spotify-sync` before
it starts building. That is not a failure: the service names an `image:`
as well as a `build:`, so Compose checks for that tag before falling back
to building it. The tag is local-only by design.

### Where configuration lives

Environment variables, everywhere. There is no config file and no dotenv
layering.

| Where | Holds |
|---|---|
| The image's `ENV` block (see `Dockerfile`) | Every non-secret default: log format, interval, both listen addresses, the redirect URL, the database path |
| `environment:` in your compose file | Anything you want to override, plus `PUID`/`PGID`/`TZ` |
| `.env.local` (gitignored) | The five secrets, in a checkout only |

`.env.local` is the *only* file to protect, and it is read by both
consumers: `mise.toml` loads it with `_.file` so `just sync` and `just
auth` see the same credentials the container does. One copy, not two to
keep in step. A missing `.env.local` is not an error, so a fresh clone
and CI still run every recipe that does not need credentials.

Two variables are worth not overriding casually, though nothing stops
you. `DIFMSYNC_DB_PATH` must point inside the mounted volume, or the
database lands in the container's ephemeral filesystem and is lost on the
next `up` — silently, because SQLite will happily create a fresh one and
the service will look like it started fine. `DIFMSYNC_AUTH_BIND` must be
`0.0.0.0`, because a published port forwards to the container's eth0
address rather than its loopback.

### Day-2 commands

Use `exec`, so you reach the container that is already running with the
volume it already has. `run` starts another one:

```sh
docker compose exec connector /difmsync status
docker compose exec connector /difmsync review
docker compose exec connector /difmsync review --approve=<difm-track-id>
docker compose exec connector /difmsync resync --forget=<id>
```

`/difmsync`, not `/app/difmsync`. `docker exec` runs as the image user,
which is root, while the service runs as `PUID` — so a command that
writes leaves root-owned files behind that the service cannot then touch.
`backup` is the one that lasts: run as root it creates `/config/backups`
root-owned `0750`, and every snapshot after that. `/difmsync` is the same
binary with the privilege drop in front, and the entrypoint's own repair
covers only the database and its sidecars, not an arbitrary path some
root process created.

If you do use `docker compose run`, do **not** add `-v difmsync-data:/config`.
Compose namespaces volumes by project, so the real one is
`<project>_difmsync-data`; naming the bare volume creates a *second*,
empty one and mounts it over `/config`. `status` then reports no account
at all, which reads as a broken deployment rather than a mistyped
command. The service already declares its volume.

## Volume ownership

**Check this first if the container crash-loops on startup.**

The service runs as `PUID:PGID` (default `1000:1000`) and the entrypoint
chowns `/config` to match on every start. For a named volume this
normally just works. It matters for a **bind mount** — the obvious move
if you want the database on a NAS or inside a snapshotted dataset:

```yaml
volumes:
  - /srv/difmsync:/config      # instead of difmsync-data:/config
```

A bind mount arrives with the ownership the host directory already has.
Set `PUID`/`PGID` to whoever owns it:

```sh
stat -c '%u %g' /srv/difmsync
```

The startup log says who it ended up as, so you can check rather than
assume:

```
difmsync-init: chown /config from 0:0 to 1000:1000
difmsync-init: starting as uid 1000 gid 1000, umask 022, TZ Etc/UTC, db /config/difmsync.db
```

If it still cannot open the database, the binary says so specifically
rather than leaving you with SQLite's bare `unable to open database file
(14)`:

```
error: sqlite.Open: ping: unable to open database file (14)
  /config has mode -rwxr-xr-x, owned by uid 0 gid 0; this process runs as uid 1000 gid 1000.
```

Prefer Docker's own `user:` (or `--user`)? That works too — the
entrypoint detects that it is already unprivileged, skips the chown, and
ignores `PUID`/`PGID`. You are then responsible for the directory's
ownership yourself.

## Upgrading

```sh
docker compose pull      # published image
docker compose up -d
```

or, from a checkout:

```sh
git pull
docker compose build
docker compose up -d
```

Migrations are embedded and applied on every boot, so there is no
separate migration step. The build needs BuildKit — the Dockerfile uses
`RUN --mount=type=cache` and `TARGETARCH`, so a host with
`DOCKER_BUILDKIT=0` fails immediately rather than subtly.

### Which tag to pull

| Tag | Moves | Use |
|---|---|---|
| `latest`, `1`, `1.2` | On each release | Normal deployments |
| `1.2.3` | Never | Pin if you want upgrades to be a file edit |
| `edge` | Every push to `main` | Testing unreleased changes |
| `sha-<short>` | Never | Pinning to an exact unreleased commit |

`latest` follows releases rather than `main`, so an unattended `docker
compose pull` gets a version tagged on purpose.

Until v1.0.0 is tagged, only `edge` and `sha-<short>` exist — the release
tags in the first two rows appear with that release.

### Upgrading from the `/data` layout

Deployments predating the `/config` change mounted the database at
`/data`. The entrypoint **refuses to start** if it finds a database there
while the configured path is empty, rather than creating a fresh one
beside it:

```
difmsync-init: found a database at /data/difmsync.db, but DIFMSYNC_DB_PATH points at
/config/difmsync.db, which does not exist.
```

That refusal is the point. A fresh database looks exactly like a working
first run — a clean startup, a consent prompt, an empty ledger — and the
refresh token and the entire sync history are in the file it ignored.

Fix it by remapping the volume, which is a one-line edit and keeps all
your state:

```yaml
volumes:
  - difmsync-data:/config      # was: difmsync-data:/data
```

Or keep the old path if you would rather not touch it:

```yaml
environment:
  DIFMSYNC_DB_PATH: /data/difmsync.db
```

After remapping, confirm before assuming:

```sh
docker compose exec connector /difmsync status   # ledger totals and watermark intact
```

The volume's contents are owned by uid 65532 if it was created by a
distroless-era image, and that uid no longer exists in this one. Both
branches above are covered on start: remapping puts the volume at
`/config`, which the entrypoint chowns, and keeping the old path is
repaired through `DIFMSYNC_DB_PATH` — the database, its sidecars and the
`/data` directory itself, which the `/config` chown never reaches.

Set `PUID`/`PGID` to what you actually want *before* the first start
either way, since that is what it chowns to.

### Deploying into an existing Compose stack

Nothing special is needed any more: pull the published image and drop the
service block from the README into your stack. The only thing to be
careful about is the volume name, if you are migrating an existing
deployment:

```yaml
volumes:
  difmsync-data:
    # Pinned, so the real volume name stops depending on which project
    # the service lives in. Compose would otherwise namespace it as
    # <yourstack>_difmsync-data — a brand-new empty one — leaving the
    # refresh token behind in the volume the old deployment created. The
    # symptom is `no account "default" yet`, and with a 30m start_period
    # that reads as a slow boot rather than a fault.
    name: difm-spotify-sync_difmsync-data
```

## First run

Always dry-run first. A one-way playlist append is tedious to undo by
hand.

```sh
docker compose exec connector /difmsync sync --dry-run
```

Read the report. Confirm the auto-add candidates are actually right, then
let the loop run. Point `DIFMSYNC_PLAYLIST_ID` at a scratch playlist for
the first live run.

**Idempotency check:** run a pass twice back to back. The second must add
zero tracks.

## Is it still working?

This is the question a homelab deployment actually needs answered,
because the sync interval is an internal ticker — nothing external
triggers a pass, so a container that stopped, wedged, or lost its
credentials simply stops syncing, quietly.

One rule answers it, and everything below uses that same rule: **the
newest pass that finished, recorded no error, and was not a dry run must
be within `DIFMSYNC_STATUS_MAX_AGE`** (45m by default — three ticks of
the 15m interval, so one missed pass is tolerated and two are not).

```sh
docker compose ps                                            # healthy / unhealthy
docker compose exec connector /difmsync status --check   # the same verdict, with a reason
curl -s http://<host>:3436/healthz                           # 200 ok, or 503 and the reason
curl -s http://<host>:3436/status.json | jq                  # the full report
```

The container healthcheck runs `/healthcheck.sh`, which is `status
--check` with a privilege drop in front of it. It is deliberately not a
curl of `/healthz`: health still works this way when
`DIFMSYNC_HTTP_ADDR` is unset, and `internal/status` is the single
implementation of the rule either way.

To see what the healthcheck itself last decided, rather than inferring
it:

```sh
docker inspect --format '{{json .State.Health}}' \
  $(docker compose ps -q connector) | jq
```

`--max-age` is a flag, so shrinking it is the cheapest way to exercise
the unhealthy path without waiting 45 minutes for a real stall:

```sh
docker compose exec connector /difmsync status --check --max-age=1s
```

That should exit non-zero and name how stale the last pass is. Note
`docker compose ps` reads `starting`, not `healthy`, for a while after
first launch — the check interval is 5m and `start_period` is 30m, which
is the pre-auth window working as intended rather than a stall.

Point a dashboard (Uptime Kuma, Homepage, anything that polls a URL) at
`/healthz`. Both endpoints are read-only and carry no secrets, which is
what makes them safe to expose on a LAN without authentication.

### When it goes red

`/healthz` and `--check` both name the reason. Match it:

| Reason | What it means | First move |
|---|---|---|
| `awaiting Spotify consent` | The daemon is up but has no refresh token | [Authorize it](#authorizing) |
| `no account "default" yet` | Nothing has ever run against this volume | Start the container; it creates the row |
| `no sync pass has run yet` | The container started but has not completed a pass | Wait one interval; then read the logs |
| `newest run errored — run …` | A pass failed and the watermark was held back | `difmsync status` for the error text |
| `last clean pass finished Nh ago` | Passes stopped completing | `docker compose logs --tail=100 connector` |
| `newest run is still in flight` | A pass is running, or was killed mid-run | Wait; if it persists, restart the container |

For the errored case, `/status.json` marks which passes failed, but **not
why** — recorded error text is deliberately kept off both endpoints,
which are served unauthenticated and would otherwise republish whatever
the failure happened to contain (DI.fm request URLs carry the member id,
for one). The text comes from the CLI, which needs the database anyway:

```sh
docker compose exec connector /difmsync status
```

A failing pass is not data loss. The watermark only advances after a
fully clean pass, so whatever the failed pass missed is re-read on the
next one.

## Backups

The database is the only copy of the Spotify refresh token, and losing it
means redoing the one interactive step in the whole system. It also holds
the ledger, the review queue and the watermark.

```sh
docker compose exec connector /difmsync backup --to=/config/backups/difmsync-$(date +%F).db
```

`$(date)` expands in *your* shell, which is what you want. That writes
into the volume — either let whatever backs up your Docker volumes pick
it up, or pull it onto the host:

```sh
docker compose cp connector:/config/backups/difmsync-$(date +%F).db ./difmsync-backup.db
```

As a nightly cron on the host:

```cron
15 4 * * * cd /srv/difm-spotify-sync && docker compose exec -T connector \
  /difmsync backup --to=/config/backups/difmsync-$(date +\%F).db
```

`-T` disables TTY allocation, which cron needs. Note the escaped `\%` —
cron treats a bare `%` as a newline.

**Pair it with a prune.** Nothing rotates these, and `VACUUM INTO` writes
a full copy every night into the same volume as the live database — so an
unpruned schedule fills that volume and then stops SQLite writing, which
takes the sync down. The backup command hardens against a full volume
(that is why it stages and verifies before publishing), but hardening
only means the *backup* fails cleanly; the database it shares the volume
with still has nowhere to write.

```cron
30 4 * * * docker compose exec -T connector \
  find /config/backups -name 'difmsync-*.db' -mtime +14 -delete
```

Fourteen days is arbitrary; size it against how much room the volume
actually has.

Three things the backup command refuses to do, all for the same reason —
the output is often the only copy of a refresh token, and restoring one
means writing it *over* the live database:

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

# Both sidecars must go. See below — this step is not optional.
docker compose run --rm --entrypoint sh connector \
  -c 'rm -f /config/difmsync.db-wal /config/difmsync.db-shm'

docker compose cp ./difmsync-backup.db connector:/config/difmsync.db
docker compose start connector
docker compose exec connector /difmsync status
```

Stop first: copying over a database with a live writer attached is how
you get a corrupt one.

`docker cp` chowns what it copies to the container's user, which is root,
and `difmsync backup` wrote the snapshot `0600` — so the restored file
lands root-owned and unreadable by the service. The entrypoint repairs
the database, its sidecars and its parent directory by name on the next
start, which is what makes the copy above safe. Note that it repairs
*those* paths: the directory chown alone would not, because it re-runs
only when `/config` itself has the wrong owner, and after a normal first
run it does not.

**The `rm` is not optional, and skipping it fails silently.** The
database runs in WAL mode, so committed pages can live in the `-wal` file
rather than in the database file. `difmsync backup` produces a single
self-contained snapshot with no WAL of its own, so copying it into place
beside a *stale* `-wal` means SQLite replays that leftover log over your
restored content on the next open. The restore is discarded, the old
database comes back, and `difmsync status` afterwards reports success —
there is nothing to notice, because as far as SQLite is concerned nothing
went wrong.

A clean `docker compose stop` checkpoints and removes the sidecars, so in
the happy path they are already gone and the `rm -f` does nothing. It
matters when the process did not exit cleanly: a SIGKILL after the 45s
grace period, a host crash, or the OOM killer (this deployment sets a
256M limit). Those are the circumstances that make you reach for a backup
in the first place, so the step belongs in the sequence rather than in a
footnote.

After a restore the ledger may be behind the playlist's real contents.
That is safe to fix: each pass reconciles against the live playlist
before adding, so `difmsync resync --forget-all` followed by a sync
rebuilds the ledger without duplicating anything.

## Recovering a deleted track

The sync never re-adds what you deleted from Spotify. To override that:

```sh
docker compose exec connector /difmsync resync --forget=<difm-track-id>
docker compose exec connector /difmsync sync
```

`--forget` on its own is the whole instruction. Two things suppress a
re-add and both have to go — the ledger row and the watermark, which
filters at *fetch* time, so a cleared ledger row alone leaves the like
unreachable. `--forget` drops the row and rewinds the watermark to one
second before that like.

Do **not** add `--all` here, even though it sounds like the thorough
choice. `--all` clears the watermark outright instead of rewinding it,
which re-reads the entire like history rather than the one track you
named. It is a much larger instruction, and it suppresses the targeted
rewind rather than adding to it.

## Rotating the DI.fm key

There is no rotation UI — the key is not surfaced anywhere in DI.fm's
settings. If it stops working (`ErrUnauthorized` in the logs), re-capture
it per [`difm-api.md`](difm-api.md) and update the secret. Sync state in
the database is unaffected; the watermark picks up where it left off.
