# difm-spotify-sync

Mirrors tracks you like on [DI.fm](https://www.di.fm) into a Spotify playlist,
unattended.

DI.fm has no public API and no export. Its "Likes" view is actually the
*voting* system filtered to upvotes — see [`docs/difm-api.md`](docs/difm-api.md)
for the reverse-engineered reference this is built on.

## Quick start

```sh
mise install          # provisions go, sqlc, goose, just, golangci-lint
cp mise.local.toml.example mise.local.toml   # then fill in credentials
just auth             # one-time Spotify consent
just dry-run          # score everything, write nothing
just sync             # for real
```

`mise.local.toml` is gitignored. Obtaining `DIFMSYNC_API_KEY` and
`DIFMSYNC_MEMBER_ID` is described in [`docs/difm-api.md`](docs/difm-api.md);
no DI.fm password is needed at any point.

Every flag has a `DIFMSYNC_*` environment fallback:

| Variable | Flag | Default |
|---|---|---|
| `DIFMSYNC_API_KEY` | `--api-key` | — (required) |
| `DIFMSYNC_MEMBER_ID` | `--member-id` | — (required) |
| `DIFMSYNC_PLAYLIST_ID` | `--playlist-id` | — (required) |
| `DIFMSYNC_SPOTIFY_CLIENT_ID` | `--spotify-client-id` | — (required) |
| `DIFMSYNC_SPOTIFY_CLIENT_SECRET` | `--spotify-client-secret` | — (required) |
| `DIFMSYNC_SPOTIFY_REDIRECT_URL` | `--spotify-redirect-url` | `http://127.0.0.1:8888/callback` |
| `DIFMSYNC_AUTH_BIND` | `--auth-bind` | the redirect URL's host |
| `DIFMSYNC_DB_PATH` | `--db-path` | `./tmp/difmsync.db` |
| `DIFMSYNC_NETWORK` | `--network` | `di` |
| `DIFMSYNC_ACCOUNT` | `--account` | `default` |
| `DIFMSYNC_INTERVAL` | `--interval` | `15m` |
| `DIFMSYNC_AUTO_THRESHOLD` | `--auto-threshold` | `0.85` |
| `DIFMSYNC_REVIEW_THRESHOLD` | `--review-threshold` | `0.60` |
| `DIFMSYNC_LOG_FORMAT` | `--log-format` | `json` |
| `DIFMSYNC_LOG_LEVEL` | `--log-level` | `info` |

`just verify-config` checks this set against the code and is part of
`just check`.

## How matching works

Radio metadata and Spotify's catalogue disagree constantly, and for
electronic music the disagreements are load-bearing: "Strobe (Extended
Mix)" and "Strobe - Radio Edit" are different recordings, minutes apart in
runtime. Taking Spotify's top search hit fills the playlist with wrong edits.

So each like is scored against up to ten candidates on four axes — title,
artist, version descriptor, and runtime — and the result routes three ways:

| Score | Outcome |
|---|---|
| exact ISRC match | added to the playlist — ISRC identifies the recording, so no scoring is needed |
| ≥ `--auto-threshold` (0.85) | added to the playlist |
| ≥ `--review-threshold` (0.60) | queued for review, with candidates and rationale |
| below that | recorded as unmatched |

Nothing is ever silently dropped. Inspect the middle bucket with
`just review`; `difmsync review --approve=<track-id>` adds the best
candidate to the playlist (`--candidate=N` picks a different one), and
`--reject` closes the item without adding.

Two details carry most of the weight. **Runtime** is the strongest signal
available — an extended mix and a radio edit of the same track differ by
minutes — and **artist similarity gates the total score**, because a
title match with the wrong artist is simply a different song.

DJ mixes and mix-show episodes are filtered out before search: an
hour-long set has no Spotify analog, and searching for one matches
something unrelated with the same name.

## Commands

```sh
difmsync auth      # one-time Spotify OAuth consent
difmsync sync      # one pass; --dry-run to write nothing; --loop to run forever
difmsync review    # inspect and resolve the review queue
difmsync resync    # reset sync state so past likes are re-evaluated
difmsync status    # ledger totals, pending count, watermark
```

Run `just` to list every recipe.

## Deployment

The binary is static and the image is distroless, so the deployment is one
container with one mounted volume:

```sh
docker compose up -d          # see compose.yaml
```

The volume at `/data` holds the SQLite database — the sync ledger, the
review queue, and the Spotify refresh token.

Full runbook — Spotify app setup, the one interactive auth step, secrets,
and verification — is in [`docs/deploy.md`](docs/deploy.md).

## Sync direction and deletions

The sync is **one-way and add-only**. That has consequences worth stating
plainly, because neither is reversible by simply running it again:

| You do this | What happens |
|---|---|
| Delete a track from the Spotify playlist | It stays deleted. Not re-added. |
| Un-like a track on DI.fm | It stays in the Spotify playlist. |
| Add a track to the playlist by hand | Untouched. The connector only manages what it added. |

Deletions sticking is deliberate — "I removed it, don't put it back" is
almost always what you meant. Making Spotify a strict mirror of DI.fm
likes is a possible future change, not current behavior.

### Getting a deleted track back

Two independent mechanisms suppress a re-add, and **both** must be cleared.
The second is the non-obvious one: the watermark filters at *fetch* time,
so dropping a ledger row alone leaves the like unreachable — it is never
retrieved to begin with.

```sh
difmsync resync --forget=<difm-track-id> --all   # drop ledger row + clear watermark
difmsync sync                                     # re-adds it
```

Find the track id with `sqlite3 "$DIFMSYNC_DB_PATH" 'select difm_track_id, artist, title from synced_tracks'`.
A track id that isn't in the ledger is reported rather than silently ignored.

`difmsync resync --forget-all` clears the whole ledger. This is safe: the
sync pass reconciles against the live playlist before adding, so tracks
already present are recorded rather than duplicated.

## Safety properties

- **Idempotent.** A unique constraint on `(account_id, difm_track_id,
  playlist_id)` — not an in-memory set — is what guarantees a second pass
  adds nothing.
- **Ledger after write.** Spotify is written first, the ledger second, so a
  crash between them re-adds rather than silently skipping.
- **Watermark last.** It advances only after a fully clean pass, so an
  interrupted run re-reads instead of losing likes.
- **Failures are recorded.** Every pass writes a `sync_runs` row including
  its error, so a silently broken sync is visible in `difmsync status`.
- **Reconciled against reality.** Each pass reads the playlist's actual
  contents, so a restored database, a cleared ledger, or a hand-added
  track cannot produce duplicates.

## Layout

```
cmd/difmsync/          urfave/cli entrypoint
pkg/difm/              AudioAddict API client (importable)
pkg/match/             normalization + scoring (pure, heavily tested)
pkg/spotify/           hand-rolled Web API client (search, playlist writes)
internal/store/sqlite/ sqlc-generated queries + typed wrappers
internal/syncer/       orchestration
migrations-sqlite/     goose migrations, embedded
docs/difm-api.md       the private-API reference this depends on
```

## Documentation

| Doc | What's in it |
|---|---|
| [`docs/difm-api.md`](docs/difm-api.md) | The reverse-engineered DI.fm API: auth, the likes endpoint, response shape, pagination gotchas |
| [`docs/deploy.md`](docs/deploy.md) | Runbook: Spotify app setup, auth step, the homelab deploy, operating it, verification |
| [`CLAUDE.md`](CLAUDE.md) | Code conventions — read before non-trivial changes |

## License

Apache 2.0. See [`LICENSE`](LICENSE).
