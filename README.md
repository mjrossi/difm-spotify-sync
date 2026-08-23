# difm-spotify-sync

[![ci](https://github.com/mjrossi/difm-spotify-sync/actions/workflows/ci.yml/badge.svg)](https://github.com/mjrossi/difm-spotify-sync/actions/workflows/ci.yml)
[![license](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)
[![ghcr](https://img.shields.io/badge/ghcr.io-difm--spotify--sync-blue)](https://github.com/mjrossi/difm-spotify-sync/pkgs/container/difm-spotify-sync)

Mirrors tracks you like on [DI.fm](https://www.di.fm) into a Spotify playlist,
unattended.

DI.fm has no public API and no export, so there is no way to get your likes
out of it. Its "Likes" view is actually the *voting* system filtered to
upvotes — see [`docs/difm-api.md`](docs/difm-api.md) for the reverse-engineered
reference this is built on.

It runs as one container, keeps its state in a SQLite database, and tells you
whether it is still working:

```
account:   default
playlist:  Radio Finds
synced:    412 track(s)
pending:   7 item(s) awaiting review
watermark: 2026-08-23T11:04:18Z
health:    ok

STARTED               DRY    ADDED  QUEUE   SKIP  ERROR
2026-08-23T11:04:02                3      1      0
2026-08-23T10:49:01                0      0      0
```

Matching radio metadata to Spotify's catalogue is the hard part, and the part
this spends its effort on: **"Strobe (Extended Mix)" and "Strobe - Radio Edit"
are different recordings**, minutes apart in runtime, and taking Spotify's top
search hit fills your playlist with the wrong ones. Anything it is not sure
about goes to a review queue instead of being guessed at.

---

## Quick start

You need a Spotify app of your own (free, two minutes) and two values captured
from a DI.fm page. Both are described under [Credentials](#credentials) below.
There is no hosted service and no account to make here.

```sh
docker run -d --name difmsync \
  -v difmsync-config:/config \
  -p 127.0.0.1:3437:3437 \
  -p 3436:3436 \
  -e PUID=1000 -e PGID=1000 -e TZ=Etc/UTC \
  -e DIFMSYNC_API_KEY=... \
  -e DIFMSYNC_MEMBER_ID=... \
  -e DIFMSYNC_SPOTIFY_CLIENT_ID=... \
  -e DIFMSYNC_SPOTIFY_CLIENT_SECRET=... \
  -e DIFMSYNC_PLAYLIST_ID=... \
  --restart unless-stopped \
  ghcr.io/mjrossi/difm-spotify-sync:latest
```

or with Compose:

```yaml
services:
  difmsync:
    image: ghcr.io/mjrossi/difm-spotify-sync:latest
    container_name: difmsync
    restart: unless-stopped
    environment:
      PUID: 1000
      PGID: 1000
      TZ: Etc/UTC
      DIFMSYNC_API_KEY: ...
      DIFMSYNC_MEMBER_ID: ...
      DIFMSYNC_SPOTIFY_CLIENT_ID: ...
      DIFMSYNC_SPOTIFY_CLIENT_SECRET: ...
      DIFMSYNC_PLAYLIST_ID: ...
    volumes:
      - difmsync-config:/config
    ports:
      - "127.0.0.1:3437:3437"   # one-time consent; inert afterwards
      - "3436:3436"             # /healthz and /status.json
    healthcheck:
      test: ["CMD", "/healthcheck.sh"]
      interval: 5m
      timeout: 10s
      retries: 3
      start_period: 30m

volumes:
  difmsync-config:
```

Then authorize once — the container waits for it rather than crash-looping:

```sh
docker logs difmsync     # -> "spotify consent required ... url=http://127.0.0.1:3437/start?t=..."
```

Open that URL **in a browser on the Docker host** and approve. Syncing starts
on the next tick. If your browser is somewhere else, see
[Authorizing](#authorizing) — there is a route for every arrangement,
including one that needs no network access at all.

Point `DIFMSYNC_PLAYLIST_ID` at a scratch playlist for the first run. The sync
is add-only and undoing a few hundred appends by hand is tedious.

## Credentials

Five values. None of them is a DI.fm password — one is never needed at any
point.

| Variable | Where it comes from |
|---|---|
| `DIFMSYNC_API_KEY` | Inlined in DI.fm's own page HTML while logged in — [`docs/difm-api.md`](docs/difm-api.md) has the one-line snippet |
| `DIFMSYNC_MEMBER_ID` | The same capture |
| `DIFMSYNC_SPOTIFY_CLIENT_ID` | An app you create at [developer.spotify.com/dashboard](https://developer.spotify.com/dashboard) |
| `DIFMSYNC_SPOTIFY_CLIENT_SECRET` | The same app |
| `DIFMSYNC_PLAYLIST_ID` | The path segment of the playlist's URL: `open.spotify.com/playlist/`**`37i9dQZF1DX...`** |

When you create the Spotify app, register `http://127.0.0.1:3437/callback` as a
redirect URI. That is what the image defaults to. Full walkthrough, including
the cases where you need a different one, is in
[`docs/deploy.md`](docs/deploy.md).

`DIFMSYNC_API_KEY` is long-lived and DI.fm exposes no way to rotate it. Treat
it like a password: it grants access to your DI.fm account, and this project
never logs it.

## Authorizing

Spotify's Authorization Code flow needs one interactive browser consent.
Everything after it is unattended — the refresh token lives in the database
and access tokens renew themselves.

The only real question is whether your browser can reach the container. Pick
the first row that describes you:

| Your situation | What to do |
|---|---|
| Browser is on the Docker host | Nothing extra. Open the URL from the logs. |
| You can SSH to the host | `ssh -L 3437:127.0.0.1:3437 <host>`, then open the same URL locally. A tunnelled `127.0.0.1` is still a loopback literal, which is what Spotify requires over plain HTTP. |
| You use Tailscale | `tailscale serve --bg --set-path /difmsync 3437`, then set `DIFMSYNC_SPOTIFY_REDIRECT_URL` to the resulting HTTPS URL and register it. |
| You run a reverse proxy | Same idea — proxy to port 3437, set the redirect URL to the public HTTPS origin, register it. |
| None of the above | `docker exec -it difmsync /app/difmsync auth --manual`. It prints a URL, you approve it in any browser anywhere, and paste back the address you land on. Nothing has to be reachable. |

Spotify requires HTTPS for any redirect URI that is not a loopback literal
(`127.0.0.1` or `[::1]`), and rejects `localhost` outright. That single rule is
why the middle rows involve TLS at all.

The consent listener exists only while there is no refresh token, and shuts
down for good once there is one. Starting a flow requires a nonce that is
generated at startup and printed once to the log, so reaching the port is not
by itself enough to bind your sync to someone else's Spotify account.

## Configuration

Every setting is an environment variable with a matching flag. The container
image ships the defaults in this table; running the binary directly gets the
CLI defaults noted where they differ.

| Variable | Flag | Default in the image |
|---|---|---|
| `DIFMSYNC_API_KEY` | `--api-key` | — (required) |
| `DIFMSYNC_MEMBER_ID` | `--member-id` | — (required) |
| `DIFMSYNC_PLAYLIST_ID` | `--playlist-id` | — (required) |
| `DIFMSYNC_SPOTIFY_CLIENT_ID` | `--spotify-client-id` | — (required) |
| `DIFMSYNC_SPOTIFY_CLIENT_SECRET` | `--spotify-client-secret` | — (required) |
| `DIFMSYNC_SPOTIFY_REDIRECT_URL` | `--spotify-redirect-url` | `http://127.0.0.1:3437/callback` (CLI: `:8888`) |
| `DIFMSYNC_DB_PATH` | `--db-path` | `/config/difmsync.db` (CLI: `./tmp/difmsync.db`) |
| `DIFMSYNC_INTERVAL` | `--interval` | `15m` |
| `DIFMSYNC_NETWORK` | `--network` | `di` |
| `DIFMSYNC_ACCOUNT` | `--account` | `default` |
| `DIFMSYNC_AUTO_THRESHOLD` | `--auto-threshold` | `0.85` |
| `DIFMSYNC_REVIEW_THRESHOLD` | `--review-threshold` | `0.60` |
| `DIFMSYNC_LOG_FORMAT` | `--log-format` | `json` |
| `DIFMSYNC_LOG_LEVEL` | `--log-level` | `info` |
| `DIFMSYNC_HTTP_ADDR` | `--http-addr` | `0.0.0.0:3436` (CLI: off) |
| `DIFMSYNC_AUTH_HTTP_ADDR` | `--auth-http-addr` | `0.0.0.0:3437` (CLI: off) |
| `DIFMSYNC_AUTH_BIND` | `--auth-bind` | `0.0.0.0` (CLI: the redirect URL's host) |
| `DIFMSYNC_STATUS_MAX_AGE` | `--max-age` | `45m` |

Container-level settings, following the usual self-hosted conventions:

| Variable | Default | What it does |
|---|---|---|
| `PUID` | `1000` | uid the service runs as, and the owner `/config` is set to |
| `PGID` | `1000` | gid, likewise |
| `UMASK` | `022` | umask for files the service creates |
| `TZ` | `UTC` | timezone for log timestamps and `difmsync status` output |

`PUID`/`PGID` matter most with a bind mount, which arrives with whatever
ownership the host directory already has. Set them to `id -u` / `id -g` for
whoever owns it. Alternatively set Docker's own `user:` and they are ignored.

`just verify-config` checks this table against the code and runs as part of
`just check`, so a variable cannot quietly stop being documented.

## How matching works

Each like is scored against up to ten Spotify candidates on four axes — title,
artist, version descriptor, and runtime — and the result routes three ways:

| Score | Outcome |
|---|---|
| exact ISRC match | added — an ISRC identifies the exact recording, so no scoring is needed |
| ≥ `--auto-threshold` (0.85) | added |
| ≥ `--review-threshold` (0.60) | queued for review, with candidates and rationale |
| below that | recorded as unmatched |

**Nothing is ever silently dropped.** Inspect the middle bucket with `difmsync
review`; `difmsync review --approve=<track-id>` adds the best candidate
(`--candidate=N` picks a different one), and `--reject` closes the item without
adding.

Two details carry most of the weight. **Runtime** is the strongest signal
available — an extended mix and a radio edit of the same track differ by
minutes — and **artist similarity gates the total score**, because a title
match with the wrong artist is simply a different song.

DJ mixes and mix-show episodes are filtered out before search: an hour-long set
has no Spotify analog, and searching for one matches something unrelated with
the same name.

## Commands

```sh
difmsync auth      # one-time Spotify consent; --manual to skip the listener
difmsync sync      # one pass; --dry-run to write nothing; --loop to run forever
difmsync review    # inspect and resolve the review queue
difmsync resync    # reset sync state so past likes are re-evaluated
difmsync status    # ledger totals, pending count, watermark, recent runs
difmsync backup    # consistent snapshot of the database
```

In a container, reach them with `docker exec difmsync /app/difmsync <command>`.

## Operating it

The sync interval is an internal ticker, so nothing external notices when a
container stops syncing. Two things answer "is it actually working?", and both
use the same rule: **the newest pass that finished, recorded no error, and was
not a dry run must be within `--max-age`.**

```sh
difmsync status            # the report, with the recent runs table
difmsync status --check    # exit 0 if healthy, non-zero with the reason
difmsync status --json     # the same report, machine-readable
```

`--check` is the container healthcheck. The daemon also serves the same verdict
over HTTP, for a dashboard:

| Endpoint | Answer |
|---|---|
| `GET /healthz` | `200 ok`, or `503` and the reason |
| `GET /status.json` | the full report; always `200`, with `"healthy": false` when it is not |

Both are **read-only and carry no secrets**, which is what makes them safe to
expose on a LAN unauthenticated. Anything that writes — approving a queued
match, for instance — stays a CLI action.

## Sync direction and deletions

The sync is **one-way and add-only**. That has consequences worth stating
plainly, because neither is reversible by simply running it again:

| You do this | What happens |
|---|---|
| Delete a track from the Spotify playlist | It stays deleted. Not re-added. |
| Un-like a track on DI.fm | It stays in the Spotify playlist. |
| Add a track to the playlist by hand | Untouched. The connector only manages what it added. |

Deletions sticking is deliberate — "I removed it, don't put it back" is almost
always what you meant. Making Spotify a strict mirror of DI.fm likes is a
possible future change, not current behavior.

### Getting a deleted track back

```sh
difmsync resync --forget=<difm-track-id>   # drops the ledger row, rewinds the watermark
difmsync sync                              # re-adds it
```

Two independent mechanisms suppress a re-add and **both** must be cleared.
The second is the non-obvious one: the watermark filters at *fetch* time, so
dropping a ledger row alone leaves the like unreachable — it is never retrieved
to begin with. `--forget` handles both by itself.

Adding `--all` is not a stronger version of the same thing: it clears the
watermark entirely, re-reading all of history instead of the one track named.

A track id that is not in the ledger is reported rather than silently
ignored, so a wrong guess is cheap. To look one up, query the database —
the image ships no `sqlite3`, so run it on the host against a bind mount or
a `difmsync backup` snapshot:

```sh
sqlite3 difmsync.db 'select difm_track_id, artist, title from synced_tracks'
```

`difmsync resync --forget-all` clears the whole ledger. This is safe: each pass
reconciles against the live playlist before adding, so tracks already present
are recorded rather than duplicated.

## Safety properties

- **Idempotent.** A unique constraint on `(account_id, difm_track_id,
  playlist_id)` — not an in-memory set — is what guarantees a second pass adds
  nothing.
- **Ledger after write.** Spotify is written first, the ledger second, so a
  crash between them re-adds rather than silently skipping.
- **Watermark last.** It advances only after a fully clean pass, so an
  interrupted run re-reads instead of losing likes.
- **Failures are recorded.** Every pass writes a `sync_runs` row including its
  error, so a silently broken sync shows up as a failing healthcheck rather
  than as nothing at all.
- **Reconciled against reality.** Each pass reads the playlist's actual
  contents, so a restored database, a cleared ledger, or a hand-added track
  cannot produce duplicates.

## Layout

```
cmd/difmsync/          urfave/cli entrypoint
pkg/difm/              AudioAddict API client (importable)
pkg/match/             normalization + scoring (pure, heavily tested)
pkg/spotify/           hand-rolled Web API client (search, playlist writes)
internal/store/sqlite/ sqlc-generated queries + typed wrappers
internal/status/       the operator report, health rule and HTTP endpoints
internal/syncer/       orchestration
migrations-sqlite/     goose migrations, embedded
docker/                container entrypoint and healthcheck
```

## Documentation

| Doc | What's in it |
|---|---|
| [`docs/deploy.md`](docs/deploy.md) | Runbook: Spotify app setup, every authorization route, upgrading, backups, recovery |
| [`docs/difm-api.md`](docs/difm-api.md) | The reverse-engineered DI.fm API: auth, the likes endpoint, response shape, pagination gotchas |
| [`CONTRIBUTING.md`](CONTRIBUTING.md) | Building it, the test gate, how to propose changes |
| [`SECURITY.md`](SECURITY.md) | Reporting a vulnerability, and the threat model of the HTTP endpoints |
| [`CLAUDE.md`](CLAUDE.md) | Code conventions — read before non-trivial changes |

## Disclaimer

This project is **not affiliated with, endorsed by, or connected to
AudioAddict, DI.fm, or Spotify** in any way. DI.fm, AudioAddict and Spotify are
trademarks of their respective owners.

It works by driving DI.fm's **private, undocumented API** — the one their own
web player uses. That API carries no compatibility promise: it can change shape,
start rejecting requests, or revoke access at any time and without notice, and
nothing here can prevent that. Whether running it is consistent with DI.fm's
terms of service is a question for you and them; that is your call to make, and
your risk to carry.

Provided as-is, without warranty of any kind — see [`LICENSE`](LICENSE).

## License

Apache 2.0. Copyright 2026 Matt Rossi. See [`LICENSE`](LICENSE).
