# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

Nothing yet. Changes land here between releases.

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
