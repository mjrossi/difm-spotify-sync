# Deploy runbook

One container, one volume. The binary is static (pure-Go SQLite driver,
`CGO_ENABLED=0`), the image is Alpine-based and drops to an unprivileged
user at startup, and everything it keeps lives in the database mounted at
`/config`.

- [Prerequisites](#prerequisites)
- [Authorizing](#authorizing) — the one interactive step
- [Running it](#running-it)
- [Volume ownership](#volume-ownership) — read this if it crash-loops
- [Upgrading](#upgrading)
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
docker compose logs difmsync | grep unrouted
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

The **nonce** in the start URL is generated when the daemon starts
waiting for consent and emitted once, to the log. Reaching the port is
not enough to begin a flow — without this, anyone who could reach it
could complete consent with *their* Spotify account and bind your sync
to a stranger's playlist. The endpoint is unauthenticated by necessity,
since you have no session with it yet.

The **callback** is guarded by the OAuth `state` parameter instead,
because Spotify redirects a browser to it and will not carry an extra
parameter. That is the standard protection, and the same one `difmsync
auth` relies on.

The listener exists only while there is no refresh token and shuts down
once there is one; it comes back only if the grant is later revoked and
the token cleared (below). A *failed* consent
deliberately leaves it up — a denied grant or a mistyped state has to be
retryable by clicking the URL again, not by restarting the container.

If Spotify later revokes the grant — a password change, or removing the
app under Spotify's *Manage apps* — the daemon notices within about an
hour (the cached access token has to expire before a refresh is
attempted, so a pass or two may first fail with a plain 401), or
immediately at its next restart if the token was already dead. It then
logs `Spotify revoked the refresh token; consent is
required again`, clears the stored token, and brings the listener back up
with a new URL and a new nonce. Click it as you did the first time;
nothing needs restarting. `auth --manual` works here too, exactly as on
first run.

## Running it

The README has the `docker run` and Compose snippets for the published
image. This repo's `compose.yaml` builds from source instead, which is
what you want if you are changing the code:

```sh
cp .env.local.example .env.local   # then fill in the five secrets
chmod 600 .env.local               # nothing does this for you
docker compose up -d
docker compose logs -f difmsync   # -> the consent URL, once
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
docker compose exec difmsync /difmsync status
docker compose exec difmsync /difmsync review
docker compose exec difmsync /difmsync review --approve=<difm-track-id>
docker compose exec difmsync /difmsync resync --forget=<id>
```

`/difmsync`, not `/app/difmsync`. `docker exec` runs as the image user,
which is root, while the service runs as `PUID` — so a command that
writes leaves root-owned files behind that the service cannot then touch.
`backup` used to be the one that lasted: run as root it created
`/config/backups` root-owned `0750`, and every snapshot after that stayed
that way until someone fixed it by hand. The entrypoint now repairs that
directory by name on every start, the same way it repairs the database
and its sidecars, so a root-owned `/config/backups` left by `/app/difmsync
backup` — or by an old host-cron job; see Backups below — is fixed on the
next restart rather than lasting indefinitely. `/difmsync` is still the
one to use day to day: it avoids creating the root-owned window at all,
rather than waiting for a restart to close it.

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

The container needs a handful of Linux capabilities to do that repair
and then drop to `PUID:PGID` — `CHOWN`, `DAC_OVERRIDE`, `SETUID` and
`SETGID` — and nothing else. Both `compose.yaml` and the README's
`docker run` snippet drop every capability first (`cap_drop: ALL`,
`no-new-privileges:true`) and add back only those four; a fifth,
`FOWNER`, was tried and left out because the entrypoint never needs it.
CI proves the set rather than assuming it stays correct: each of the
four is checked with its own negative control in
`container-tests.yml` — dropping any one of them fails a specific,
identifiable step, not a generic permission error.

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

`edge` and `sha-<short>` are published from `main` between releases, so
they exist only after a push to `main` — a freshly pruned registry may
have neither until the next merge. Deployments should be on `latest` or a
pinned version regardless; those are the tags a release publishes.

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
docker compose exec difmsync /difmsync sync --dry-run
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
be within `DIFMSYNC_STATUS_MAX_AGE`** (unset, three times
`DIFMSYNC_INTERVAL` — 45m at the default 15m — so one missed pass is
tolerated and two are not, whatever the interval; set it to override).

```sh
docker compose ps                                            # healthy / unhealthy
docker compose exec difmsync /difmsync status --check   # the same verdict, with a reason
curl -s http://<host>:3436/healthz                           # 200 ok, or 503 and the reason
curl -s http://<host>:3436/status.json | jq                  # the full report
```

The JSON report's own fields worth knowing: `healthy` and `reason` are
the same verdict `/healthz` gives; `version` names the build that
answered; `last_success_at` is the finished time of the pass the health
rule accepted, absent once that pass has fallen out of the last 20
`sync_runs` rows; `consecutive_failures` counts errored passes since
then, capped at 20 (`20` means "at least 20"); and `runs[].error_kind`
names why each recent pass failed, never the error text.

The container healthcheck runs `/healthcheck.sh`, which is `status
--check` with a privilege drop in front of it. It is deliberately not a
curl of `/healthz`: health still works this way when
`DIFMSYNC_HTTP_ADDR` is unset, and `internal/status` is the single
implementation of the rule either way.

To see what the healthcheck itself last decided, rather than inferring
it:

```sh
docker inspect --format '{{json .State.Health}}' \
  $(docker compose ps -q difmsync) | jq
```

`--max-age` is a flag, so shrinking it is the cheapest way to exercise
the unhealthy path without waiting 45 minutes for a real stall:

```sh
docker compose exec difmsync /difmsync status --check --max-age=1s
```

That should exit non-zero and name how stale the last pass is. Note
`docker compose ps` reads `starting`, not `healthy`, for a while after
first launch — the check interval is 5m and `start_period` is 30m, which
is the pre-auth window working as intended rather than a stall.

Point a dashboard (Uptime Kuma, Homepage, anything that polls a URL) at
`/healthz`. Both endpoints are read-only and carry no secrets, which is
what makes them safe to expose on a LAN without authentication.

Reading `docker compose logs -f difmsync` directly, a healthy idle
interval shows as one `pass finished` line per tick, carrying `next_run`
for when the next one fires — nothing more, unless a like was actually
fetched. A pass that swallowed something logs `clean=false` on that same
line and names the `kind` alongside it, matching the reason table below.

### When it goes red

`/healthz` and `--check` both name the reason. Match it:

| Reason | What it means | First move |
|---|---|---|
| `awaiting Spotify consent` | The daemon is up but has no refresh token | [Authorize it](#authorizing) |
| `no account "default" yet` | Nothing has ever run against this volume | Start the container; it creates the row |
| `no sync pass has run yet` | The container started but has not completed a pass | Wait one interval; then read the logs |
| `newest run errored — run …` | A pass failed and the watermark was held back | `difmsync status` for the error text |
| `awaiting Spotify consent` (after a revoked grant) | The refresh token was rejected; the daemon cleared it and brought the consent server back up. The `KIND` column / `error_kind` says `spotify_grant_revoked` | Open the new consent URL from the log |
| `newest run found the Spotify grant revoked` | Consent was re-given (by you, or a sidecar `auth --manual`) and the first pass since has not completed yet | Wait one interval; if it persists, open the consent URL or run `difmsync auth` |
| `newest run had its DI.fm API key rejected` | `DIFMSYNC_API_KEY` no longer works | [Rotate the key](#rotating-the-difm-key) |
| `newest run was rate limited` | An API answered 429; the loop backs off by its `Retry-After` | Nothing — it recovers on its own |
| `last clean pass finished Nh ago` | Passes stopped completing | `docker compose logs --tail=100 difmsync` |
| `newest run is still in flight` | A pass is running, or was killed mid-run | Wait; if it persists, restart the container |

For the errored case, `/status.json` marks which passes failed, but **not
why** — recorded error text is deliberately kept off both endpoints,
which are served unauthenticated and would otherwise republish whatever
the failure happened to contain (DI.fm request URLs carry the member id,
for one). The text comes from the CLI, which needs the database anyway:

```sh
docker compose exec difmsync /difmsync status
```

A failing pass is not data loss. The watermark only advances after a
fully clean pass, so whatever the failed pass missed is re-read on the
next one.

## Backups

The database is the only copy of the Spotify refresh token, and losing it
means redoing the one interactive step in the whole system. It also holds
the ledger, the review queue and the watermark.

Run history is pruned to 90 days on every clean pass, so a snapshot
carries at most that much of `sync_runs` — the ledger, review queue and
watermark are what a restore actually depends on, and none of those are
pruned.

**The daemon backs itself up.** After every clean, non-dry sync pass it
takes one verified snapshot per UTC day into `DIFMSYNC_BACKUP_DIR` (the
published image defaults this to `/config/backups`; set it explicitly if
you run from a checkout, where the CLI default is off) and keeps
`DIFMSYNC_BACKUP_KEEP` of them, oldest first (default 14; `0` keeps every
one). There is nothing to schedule — no cron, no sidecar, no
`docker exec` on a timer — and the result is owned by the service, not
root, because the service writes it itself.

Snapshots are named `difmsync-YYYY-MM-DD.db`. That name is also how both
the prune and `difmsync status` recognize one — each parses the date out
of it rather than just matching the prefix and suffix. **A file in that
directory under any other name is left strictly alone**: never deleted
by the prune, and never reported as the last backup. `docker cp` a copy
in from elsewhere, or save a manual snapshot as
`difmsync-before-upgrade.db`, and it sits there indefinitely — which is
the point, since the whole reason to give it a distinct name is so the
automatic prune won't later delete it out from under you. (Naming a
manual snapshot in the exact `difmsync-YYYY-MM-DD.db` shape does the
opposite: it becomes indistinguishable from an automatic one and is
eligible for pruning like any other.)

A failed attempt — a full volume, a directory the daemon can't write to
— still counts as that day's attempt, so a standing problem warns once a
day in the logs rather than once a pass. It's retried the next UTC day,
or sooner if the container restarts; the "did we already try today"
marker lives only in memory.

`difmsync status` (and `--json`, and `/status.json`) reports the newest
snapshot's date as `last backup:`. `last backup: none` means a directory
is configured but holds no snapshot yet — check the logs for a warning
rather than assuming one is about to appear.

A one-shot `difmsync sync` (run without `--loop`, the way a
`docker compose run` debugging invocation would) still writes the day's
snapshot, but never prunes — so it can't quietly trim the retention the
running daemon is managing.

**Migration note.** If an earlier deployment ran a host cron job for
backups, that cron used `docker compose exec`, which runs as root, so
`/config/backups` and everything in it ended up root-owned. Nothing to
do about that by hand: the entrypoint now repairs that directory, by
name, on the next container start — the same way it already repairs the
database and its sidecars — so an upgrade fixes it automatically.

**Remove the cron entry.** It does not merely duplicate the daemon: it
writes the same `difmsync-YYYY-MM-DD.db` name, so on any day the daemon
has already taken its snapshot, the cron's `backup` finds the file there,
refuses to overwrite it, and exits non-zero, every night. Its `find
-mtime` prune is redundant with `DIFMSYNC_BACKUP_KEEP` as well.

For an on-demand copy — before an upgrade, or to pull one off the host by
hand — `difmsync backup --to=<path>` is still there:

```sh
docker compose exec difmsync /difmsync backup --to=/config/backups/difmsync-before-upgrade.db
docker compose cp difmsync:/config/backups/difmsync-before-upgrade.db ./difmsync-backup.db
```

`/difmsync`, not `/app/difmsync` — see Day-2 commands above; going
through the bare binary via `docker exec` creates the file as root.

Both routes — the daemon's own daily snapshot and `backup --to` — go
through the same verified-snapshot routine, so three things are refused
either way, all for the same reason: the output is often the only copy
of a refresh token, and restoring one means writing it *over* the live
database:

- **Overwrite an existing destination.** Pick another `--to`, or move the
  old file away first.
- **Leave a snapshot it could not verify.** It reopens the result and
  reads the account row back. A file that fails is deleted, because a
  plausible-looking file left behind is how it gets restored later by
  someone who never saw the error.
- **Write it world-readable.** The snapshot is `chmod 600`.

It also refuses to run at all against a corrupt *source*: opening the live
database first runs the same integrity check described under Restoring
below, so a damaged database fails before `backup` writes anything —
never as a snapshot that opens fine and only turns out empty or wrong
later. That is the check doing its job, not a backup regression; if it
happens, the source database is already damaged and the answer is your
last good backup, not this command.

### Restoring

```sh
docker compose stop difmsync

# Both sidecars must go. See below — this step is not optional.
docker compose run --rm --entrypoint sh difmsync \
  -c 'rm -f /config/difmsync.db-wal /config/difmsync.db-shm'

docker compose cp ./difmsync-backup.db difmsync:/config/difmsync.db
docker compose start difmsync
docker compose exec difmsync /difmsync status
```

Stop first: copying over a database with a live writer attached is how
you get a corrupt one.

If the file you copied in is itself bad — a short or interrupted copy, or
one that landed corrupt in place — the daemon refuses it at startup
rather than crash-looping partway into a query. Both shapes are caught,
by two different checks, but they end in the same message:

```
sqlite.Open: /config/difmsync.db: sqlite: database is unreadable (<reason>);
restore from a backup — see the Restoring section of the deployment runbook,
https://github.com/mjrossi/difm-spotify-sync/blob/main/docs/deploy.md#restoring
```

`<reason>` differs — a truncated or not-a-database file gives SQLite's own
open error, an in-place-corrupt one gives its `quick_check` diagnosis —
but the fix is the same either way: get a fresh copy of the backup, don't
retry the file that's already in `/config`. The healthcheck opens the
database the same way, so this also shows up as an unhealthy container,
not only as a startup crash.

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
docker compose exec difmsync /difmsync resync --forget=<difm-track-id>
docker compose exec difmsync /difmsync sync
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
