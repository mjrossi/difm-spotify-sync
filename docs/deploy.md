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
2. Add the redirect URIs. Each must match `DIFMSYNC_SPOTIFY_REDIRECT_URL`
   exactly for the context that uses it, and the dashboard accepts more
   than one — register both:
   - `http://127.0.0.1:8888/callback` — the default, used by `just auth`
     on a workstation.
   - `https://<host>/difmsync/callback` — the deployed daemon, if you are
     using the in-daemon consent flow below.

   Spotify requires HTTPS for any redirect URI that is not a loopback
   literal (`127.0.0.1` or `[::1]`); `localhost` is rejected outright.
   That is why the deployed URL has to be fronted by something that
   terminates TLS, and why the workstation one stays on `127.0.0.1`.
3. Copy the client ID and secret.

The playlist ID is the path segment in its URL:
`https://open.spotify.com/playlist/`**`37i9dQZF1DX...`**

## The auth step

Spotify's Authorization Code flow needs one interactive browser consent.
Everything after it is unattended — the refresh token is stored in the
SQLite database and the OAuth transport renews access tokens on its own.

There are two ways to give that consent. They write the same token to the
same place; pick whichever suits where the database lives.

### In the daemon (deployed)

Set `DIFMSYNC_AUTH_HTTP_ADDR` and the daemon handles it itself. While
there is no refresh token it serves the consent flow on that address and
waits, logging one line:

```
spotify consent required — open this URL to authorize url=https://nas.tail1234.ts.net/difmsync/start?t=<nonce>
```

Open it, approve, and syncing begins on the next tick. The listener then
shuts down for the life of the process.

Two things make this safe enough to leave exposed for the minutes it is
up. The start URL carries a **nonce** generated at startup and emitted
only to the log, so reaching the port is not enough to begin a flow —
without it, anyone who could reach the port could complete consent with
*their* Spotify account and bind your sync to a stranger's playlist. The
callback itself is guarded by the OAuth `state` parameter, as it must be:
Spotify redirects a browser there and will not carry an extra parameter.

Both served paths are derived from `DIFMSYNC_SPOTIFY_REDIRECT_URL`, so
scoping it to `/difmsync/callback` puts the start page at
`/difmsync/start` rather than claiming `/callback` at the root of a
hostname that may front several services.

**Unset, nothing changes**: the daemon exits with `spotify: no refresh
token; run difmsync auth first`, which is what a workstation wants.

#### Fronting it with Tailscale

`DIFMSYNC_AUTH_HTTP_ADDR` serves plain HTTP, and `compose.yaml` publishes
it to the host's loopback only. Something has to terminate TLS in front of
it, both so a browser elsewhere can reach it and because Spotify will not
accept a non-loopback redirect URI over HTTP. `tailscale serve` does both
with a real certificate and no ports open to the internet:

```sh
tailscale serve --bg --set-path /difmsync 3437
tailscale serve status          # confirm the mapping
```

Then set, in `.env.local` on that host:

```sh
DIFMSYNC_SPOTIFY_REDIRECT_URL=https://<node>.<tailnet>.ts.net/difmsync/callback
```

and register that same URL in the Spotify dashboard.

Whether a path-mounted proxy passes its prefix through to the backend or
strips it has changed across Tailscale releases. The daemon answers on
both the full path and its last segment for exactly this reason — a
mismatch would 404 the callback, which is indistinguishable from Spotify
never calling back. Anything that reaches neither is logged with the path
it asked for, so a rewrite leaves evidence:

```sh
docker compose logs connector | grep unrouted
```

### On a workstation

```sh
just auth        # prints a URL, waits on 127.0.0.1:8888 for the callback
```

The listener's address and path are both derived from
`DIFMSYNC_SPOTIFY_REDIRECT_URL`, so a custom redirect URI works as long
as it matches what you registered in the Spotify dashboard. This path
requires an `http` loopback redirect URL and will refuse an `https` one —
it serves plain HTTP and cannot terminate TLS. **Make sure the database it
writes to is the one the deployment will mount.**

## Homelab (Docker Compose)

```sh
cp .env.local.example .env.local   # then fill in the five secrets
chmod 600 .env.local               # nothing does this for you
docker compose up -d
docker compose logs -f connector   # -> the consent URL, once
```

The first build prints `pull access denied for difm-spotify-sync,
repository does not exist or may require 'docker login'` before it starts
building. That is not a failure: the service names an `image:` as well as
a `build:`, so Compose checks the registry for that tag before falling
back to building it locally. The tag is local-only by design — there is
no registry to push to. Once the image exists the line does not reappear.

### Where configuration lives

The dotenv files layer the same way the mise ones do, and the `<env>`
layer is chosen by the same `MISE_ENV` switch:

| Layer | Mirrors | Committed | Holds |
|---|---|---|---|
| `.env.defaults` | `mise.toml` | yes | non-secret container defaults |
| `.env.<env>` | `mise.<env>.toml` | yes | per-environment non-secrets |
| `.env.local` | `mise.local.toml` | **no** | the five secrets |
| `.env.<env>.local` | `mise.<env>.local.toml` | **no** | per-environment secrets |

Later layers win, and only `.env.defaults` has to exist. Unset `MISE_ENV`
means production, matching `mise.toml`'s role, so the plain command is the
deployment one:

```sh
MISE_ENV=production docker compose up -d      # production
MISE_ENV=development docker compose up -d     # text logs, debug, 2m interval
```

Set it explicitly on the deploy, rather than relying on the `:-production`
default. `MISE_ENV` is read from *your* shell, and any shell with mise
active for this repo already exports `MISE_ENV=development` — so a
`docker compose up -d` typed in a working checkout picks up the
development layer and deploys a **2m** sync interval against a private,
undocumented API, roughly seven times the intended rate, with debug logs
to match. It works, which is what makes it easy to miss.

To confirm which layer landed, read the interval back out of the startup
log — `Loop` records the effective value:

```sh
docker compose logs connector | grep -m1 interval
```

Two variables are pinned in `compose.yaml`'s `environment:` block instead,
because that block overrides every layer: `DIFMSYNC_DB_PATH` (the volume
mount point) and `DIFMSYNC_AUTH_BIND` (`0.0.0.0`, since a published port
forwards to eth0 rather than loopback). Both are facts about running in
this container rather than preferences, and both fail in ways that are
tedious to diagnose — a wrong `DB_PATH` writes the database into the
container's ephemeral filesystem and looks like a clean start. Setting
either in a `.env` layer has no effect, by design.

`.env.local` is also what the *host* toolchain reads: `mise.toml` loads it
with `_.file`, so `just sync` and `just auth` see the same credentials the
container does. There is one secrets file, not two to keep in step.

A missing `.env.local` is not an error — CI and a fresh clone still run
every recipe that does not need credentials. And because editing
`mise.toml` changes its content hash, mise will ask you to `mise trust`
once after pulling this change.

### Consent inside the container, the long way

The in-daemon flow above replaces this, but `docker compose run --rm
--service-ports connector auth` still works and is the fallback when you
would rather not expose a consent port at all. Two things about it, both
easy to get wrong:

- **`--service-ports` is required.** `docker compose run` does not publish
  the service's ports without it, so the `ports:` block in `compose.yaml`
  has no effect on that command and the callback never arrives. Note it
  publishes *every* port in that block, so stop the daemon first or it
  collides with the running container on 3436.
- **The listener must bind `0.0.0.0` inside the container.** A published
  port forwards to the container's eth0 address, not its loopback, so a
  listener on `127.0.0.1` is unreachable from the host. `compose.yaml`
  sets `DIFMSYNC_AUTH_BIND=0.0.0.0` for this. The redirect URL stays a
  loopback literal — that is what your browser opens and what Spotify's
  dashboard requires — so reaching it from another machine means an ssh
  tunnel to the published port.

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
docker compose exec connector /app/difmsync resync --forget=<id>
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

### Deploying into an existing Compose stack

Everything above assumes this repo *is* the Compose project — `build: .`,
`env_file` paths relative to the checkout, and a volume Compose namespaces
`difm-spotify-sync_difmsync-data`. Adding the service to a homelab stack
that already runs other things breaks all three at once, so pull a
published image instead of building in place.

CI publishes to GHCR on every push to `main`:

| Tag | Use |
|---|---|
| `ghcr.io/mjrossi/difm-spotify-sync:latest` | tracks `main` |
| `ghcr.io/mjrossi/difm-spotify-sync:sha-<short>` | immutable; what to pin |

The package is **private**, because the repo is. One-time login on the
homelab host, with a classic PAT carrying `read:packages` — the workflow's
`GITHUB_TOKEN` writes it, but that token exists only inside Actions and
cannot be used to pull:

```sh
printf '%s' "$GHCR_PAT" | docker login ghcr.io -u mjrossi --password-stdin
```

Then the service block, dropped into your stack's `compose.yaml`:

```yaml
  difmsync:
    # Pin the sha in production and bump it deliberately. `latest` moves
    # under you on the next merge, which is the one thing a homelab
    # deployment should never do unattended.
    image: ghcr.io/mjrossi/difm-spotify-sync:sha-abc1234
    restart: unless-stopped

    # Two layers, not four. The homelab is always production, so the
    # `.env.${MISE_ENV:-production}` layer is dropped along with the
    # interpolation — which also removes the footgun described above,
    # where a shell with mise active exports MISE_ENV=development and
    # silently deploys a 2m interval. There is no MISE_ENV to read here.
    #
    # Copy .env.defaults out of the repo to sit beside this file. It is
    # committed and secret-free, but it is now a *copy* — re-copy it when
    # upgrading, or a default added upstream silently never arrives.
    env_file:
      - ./difmsync/.env.defaults
      - ./difmsync/.env.local

    # Same two pins as the standalone compose.yaml, and for the same
    # reason: this block overrides every env_file layer, and both values
    # are facts about running in this container rather than preferences.
    environment:
      DIFMSYNC_DB_PATH: /data/difmsync.db
      DIFMSYNC_AUTH_BIND: 0.0.0.0

    volumes:
      - difmsync-data:/data
    ports:
      - "127.0.0.1:3437:3437"   # consent flow; inert after consent
      - "3436:3436"             # /healthz and /status.json
    healthcheck:
      test: ["CMD", "/app/difmsync", "status", "--check"]
      interval: 5m
      timeout: 10s
      retries: 3
      start_period: 30m
    stop_grace_period: 45s
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"
    deploy:
      resources:
        limits:
          memory: 256M
          cpus: "0.5"

volumes:
  difmsync-data:
    # Pinned, so the real volume name stops depending on which project the
    # service lives in.
    #
    # This is the migration. Compose would otherwise namespace the volume
    # as <yourstack>_difmsync-data — a brand-new empty one — leaving the
    # Spotify refresh token behind in the volume the standalone deployment
    # created. The symptom is `no account "default" yet`, and with a 30m
    # start_period that reads as a slow boot rather than a fault. Naming
    # the existing volume outright means there is nothing to copy.
    #
    # It also keeps the literal volume names in the backup, prune and
    # restore commands below correct, which project-prefixing would not.
    name: difm-spotify-sync_difmsync-data
```

Alongside it:

```sh
mkdir -p difmsync
cp /path/to/repo/.env.defaults difmsync/.env.defaults
cp /path/to/repo/.env.local    difmsync/.env.local   # the five secrets
chmod 600 difmsync/.env.local
```

If you would rather the volume were named for the stack it now lives in,
that is a copy rather than a rename — stop the container first, since
copying a database out from under a live writer is how you get a corrupt
one:

```sh
docker compose stop difmsync
docker volume create difmsync-data
docker run --rm \
  -v difm-spotify-sync_difmsync-data:/from -v difmsync-data:/to \
  alpine sh -c 'cd /from && cp -a . /to'
```

`cp -a` preserves the uid 65532 ownership, which the nonroot process
needs — see [Volume ownership](#volume-ownership). Point `name:` at the
new volume, `docker compose up -d`, confirm with `difmsync status`, and
only then remove the old one.

Two things change in the commands above once the service lives here: it is
`difmsync` rather than `connector`, and you run them from the stack's
directory. The one-time consent step is otherwise unchanged, and still
needs `--service-ports`:

```sh
docker compose run --rm --service-ports difmsync auth
```

Upgrading no longer builds anything:

```sh
docker compose pull difmsync     # only for :latest; a pinned sha is a file edit
docker compose up -d difmsync
```

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
still works if `DIFMSYNC_HTTP_ADDR` is unset. To see what the healthcheck
itself last decided, rather than inferring it:

```sh
docker inspect --format '{{json .State.Health}}' \
  $(docker compose ps -q connector) | jq
```

`--max-age` is a flag, so shrinking it is the cheapest way to exercise the
unhealthy path without waiting 45 minutes for a real stall:

```sh
docker compose exec connector /app/difmsync status --check --max-age=1s
```

That should exit non-zero and name how stale the last pass is. Note
`docker compose ps` reads `starting`, not `healthy`, for a while after
first launch — the check interval is 5m and `start_period` is 30m, which
is the pre-auth window working as intended rather than a stall.

Point a dashboard (Uptime Kuma, Homepage, anything that polls a URL) at
`/healthz`. Both endpoints are read-only and carry no secrets, which is
what makes them safe to expose on the LAN without authentication.

### When it goes red

`/healthz` and `--check` both name the reason. Match it:

| Reason | What it means | First move |
|---|---|---|
| `awaiting Spotify consent` | The daemon is up but has no refresh token | Open the consent URL from the log |
| `no account "default" yet` | Nothing has ever run against this volume | Start the container; it creates the row |
| `no sync pass has run yet` | The container started but has not completed a pass | Wait one interval; then read the logs |
| `newest run errored — run …` | A pass failed and the watermark was held back | `difmsync status` on the host for the error text |
| `last clean pass finished Nh ago` | Passes stopped completing | `docker compose logs --tail=100 connector` |
| `newest run is still in flight` | A pass is running, or was killed mid-run | Wait; if it persists, restart the container |

For the errored case, `/status.json` marks which passes failed, but
**not why** — recorded error text is deliberately kept off both endpoints,
which are served to the LAN unauthenticated and would otherwise republish
whatever the failure happened to contain (DI.fm request URLs carry the
member id, for one):

```sh
curl -s http://<host>:3436/status.json | jq '.runs[] | {started_at, added, failed}'
```

The text itself comes from the CLI, which needs the database anyway:

```sh
docker compose exec connector /app/difmsync status
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
cron treats a bare `%` as a newline.

**Pair it with a prune.** Nothing rotates these, and `VACUUM INTO` writes
a full copy every night into the same volume as the live database — so an
unpruned schedule fills that volume and then stops SQLite writing, which
takes the sync down. The backup command hardens against a full volume
(that is why it stages and verifies before publishing), but hardening only
means the *backup* fails cleanly; the database it shares the volume with
still has nowhere to write.

The image has no shell, so the prune runs from a throwaway container
against the same named volume:

```cron
30 4 * * * docker run --rm -v difm-spotify-sync_difmsync-data:/data alpine \
  find /data/backups -name 'difmsync-*.db' -mtime +14 -delete
```

Check the volume's real name with `docker volume ls` — Compose prefixes it
with the project directory. Fourteen days is arbitrary; size it against
how much room the volume actually has.

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

# Both sidecars must go, and the container is stopped — so do it from a
# throwaway container on the same volume. Confirm the volume name with
# `docker volume ls`; Compose namespaces it by project directory.
docker run --rm -v difm-spotify-sync_difmsync-data:/data alpine \
  rm -f /data/difmsync.db-wal /data/difmsync.db-shm

docker compose cp ./difmsync-backup.db connector:/data/difmsync.db
docker compose start connector
docker compose exec connector /app/difmsync status
```

Stop first: copying over a database with a live writer attached is how you
get a corrupt one. If `/data` is a bind mount, `chown 65532:65532` the
restored file — see [Volume ownership](#volume-ownership).

**The `rm` is not optional, and skipping it fails silently.** The database
runs in WAL mode, so committed pages can live in the `-wal` file rather
than in the database file. `difmsync backup` produces a single
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
That is safe to fix: each pass reconciles against the live playlist before
adding, so `difmsync resync --forget-all` followed by a sync rebuilds the
ledger without duplicating anything.

## Recovering a deleted track

The sync never re-adds what you deleted from Spotify. To override that:

```sh
sqlite3 "$DIFMSYNC_DB_PATH" 'select difm_track_id, artist, title from synced_tracks;'
difmsync resync --forget=<difm-track-id>
difmsync sync
```

`--forget` on its own is the whole instruction. Two things suppress a
re-add and both have to go — the ledger row and the watermark, which
filters at *fetch* time, so a cleared ledger row alone leaves the like
unreachable. `--forget` handles both: it drops the row and rewinds the
watermark to one second before that like.

Do **not** add `--all` here, even though it sounds like the thorough
choice. `--all` clears the watermark outright instead of rewinding it,
which re-reads the entire like history rather than the one track you
named. It is a much larger instruction, and it suppresses the targeted
rewind rather than adding to it.

To rebuild the ledger from scratch — after restoring a backup, say —
`difmsync resync --forget-all` then `difmsync sync`. No duplicates result;
the pass reconciles against the playlist's real contents first.

## Rotating the DI.fm key

There is no rotation UI — the key is not surfaced anywhere in DI.fm's
settings. If it stops working (`ErrUnauthorized` in the logs), re-capture it
per [`difm-api.md`](difm-api.md) and update the secret. Sync state in the
database is unaffected; the watermark picks up where it left off.
