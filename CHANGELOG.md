# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `sqlite.Open` now refuses a database SQLite cannot read — a truncated
  or non-SQLite file, or one that fails `PRAGMA quick_check` in place —
  instead of surfacing as whatever query happens to touch it first. The
  error names the file and points at the Restoring section of
  `docs/deploy.md`. The healthcheck and `backup` open the database the
  same way, so a corrupt file now fails `status --check`/`/healthz` and
  `backup` before either does anything else.
- `just fuzz` runs the matcher's fuzz targets (`Normalize`, `Parse`,
  `Score`) for a bounded time; their seed corpus already runs as ordinary
  test cases in `just check`.

### Changed

- `sync_runs` is pruned after each clean pass: 90 days of history, never
  fewer than the newest 20 rows (the health scan window). Not
  configurable — see CLAUDE.md, Sync semantics.

- `DIFMSYNC_STATUS_MAX_AGE`, left unset, now follows the interval — three
  times `DIFMSYNC_INTERVAL` — instead of a fixed 45m regardless of it, so
  a longer interval no longer reports unhealthy between every pair of
  passes. The declared default is still `45m`; set the variable to
  override the derivation.
- The loop logs one `pass finished` line per pass at Info, with the
  fetch/add/queue/skip counts, whether it was clean, and `next_run` —
  replacing the two lines an idle tick used to log without ever saying
  when the next attempt was.
- `difmsync status`, `--json` and `/status.json` gain `version`,
  `last_success_at` (the accepted pass's own finish time) and
  `consecutive_failures` (capped at the 20-row scan window), so an
  operator or a dashboard can see which build answered and how long a
  stall has been running without reading the runs table by hand.
- A refresh token that Spotify revokes mid-life no longer needs a human
  to run `difmsync auth` and restart the container. The daemon clears the
  dead token, brings the consent server back up with a fresh URL and
  nonce, and resumes once you click it.
- A 429 from either API now delays the next pass by the `Retry-After`
  the server sent (clamped to 1m–24h) instead of a full interval.
- A rejected DI.fm API key gets its own log line and its own `/healthz`
  reason, rather than the generic "newest run errored".
- `difmsync status` and `/status.json` carry an `error_kind` for failed
  passes — a fixed category, never the error text, which stays CLI-only.
- A one-shot `difmsync sync` with a revoked grant now exits non-zero from
  the playlist probe, before the pass runs and without recording a
  `sync_runs` row; it used to warn and run the pass anyway.

### Fixed

- The 20-row health scan window is now fixed in both directions. A large
  `--limit` used to widen it, so `difmsync status --limit 50` could
  report healthy in a case where `/healthz` — which always uses the
  fixed window — reported unhealthy.
- An artist whose name is or begins with a separator word (`X
  Ambassadors`, `And One`, `With Confidence`) was parsed with that word
  dropped. This both prevented genuine matches and, worse, could let a
  collaboration auto-add as one member's solo track — `X & Beta`
  parsed as just `Beta` and matched that artist's recording at full
  confidence.

### Database

- Migration `0002` adds `sync_runs.error_kind`. Applied automatically on
  start; existing rows read as unclassified.

## [1.0.0] - 2026-08-25

First public release. There is no upgrade path to document because there is no
earlier version — everything below describes what 1.0.0 does, not what changed.

### Sync

- One-way, add-only mirroring of DI.fm liked tracks into a Spotify playlist.
  Spotify deletions are not reverted and DI.fm un-likes are not propagated.
- Matching scores title, artist, duration and version (Original / Extended /
  Radio Edit / Remix) rather than taking Spotify's top hit. An ISRC, when DI.fm
  supplies one, settles the match outright. Anything below the auto threshold
  goes to a review queue instead of being guessed at.
- A crash between any two steps causes a redundant re-read, never a silent
  skip: the Spotify write, the ledger row and the watermark advance in that
  order, and the watermark moves only after a fully clean pass.
- `resync` recovers individual tracks (`--forget=<id>`), the whole ledger
  (`--forget-all`), or re-runs everything (`--all`).

### Running it

- One container, one volume at `/config`, published multi-arch (`linux/amd64`,
  `linux/arm64`) to GHCR.
- Configuration is environment variables throughout, with non-secret defaults
  baked into the image — `docker run` with the five credentials is a whole
  deployment, with no config file to copy.
- `PUID` / `PGID` / `UMASK` / `TZ` are honoured, following the conventions
  self-hosted images generally use. The entrypoint applies them and drops to
  that user with `exec su-exec`, so the service is pid 1 and receives `SIGTERM`
  directly.
- A `HEALTHCHECK` in the image itself, so both `docker run` and Compose report
  health without configuring anything. Nothing external triggers a sync pass —
  the interval is an internal ticker — so an unhealthy container is the signal
  that syncing stopped.
- `/difmsync` is the privilege-dropping way to run commands with `docker exec`,
  which runs as root.

### Authorizing

Spotify's Authorization Code flow needs one interactive browser consent, and
three routes reach it, so that no deployment arrangement is locked out:

- The daemon's own consent server, which runs only while there is no refresh
  token and shuts down for the life of the process once one is stored. Starting
  a flow requires a nonce emitted once to the log; the callback is guarded by
  the OAuth `state` parameter.
- `difmsync auth`, which binds a loopback listener.
- `difmsync auth --manual`, which binds nothing at all — the redirect URI only
  has to be registered with Spotify, not reachable. This is what makes a NAS, a
  VPS, or a host behind CGNAT workable.

### Observability

- `GET /healthz` and `GET /status.json`, both read-only and carrying no
  secrets, for a dashboard to poll.
- `difmsync status --check` applies the same health rule from the CLI, and is
  what the container healthcheck runs.
- `difmsync backup` takes a consistent snapshot without stopping the service.
- `difmsync --version` reports the build. Released images carry the git tag;
  a build from a checkout reports its commit.

[Unreleased]: https://github.com/mjrossi/difm-spotify-sync/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/mjrossi/difm-spotify-sync/releases/tag/v1.0.0
