# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `difmsync auth --manual`: paste-the-URL consent for deployments where no
  browser can reach the container. The redirect URI only has to be registered
  with Spotify, not reachable, so this works on a NAS, a VPS, or behind CGNAT.
- `PUID` / `PGID` / `UMASK` / `TZ` support in the container image, following
  the conventions self-hosted images generally use.
- Multi-architecture images (`linux/amd64`, `linux/arm64`) published to GHCR.
- Tagged releases: `latest` now tracks the newest release rather than `main`,
  which publishes `edge` and `sha-<short>`.
- `CONTRIBUTING.md`, `SECURITY.md`, `NOTICE`, and this file.
- A `HEALTHCHECK` in the image itself, so `docker run` deployments report
  health without configuring anything. It was previously only in `compose.yaml`.
- `/difmsync` in the image: the privilege-dropping way to run commands with
  `docker exec`, which runs as root. Use it in place of `/app/difmsync`.

### Changed

- The image is now Alpine-based rather than distroless, so it can apply
  `PUID`/`PGID` before dropping privileges. It starts as root and drops to
  `PUID:PGID` via `su-exec`, with `exec`, so the service is still pid 1 and
  receives `SIGTERM` directly.
- The database lives at `/config/difmsync.db` rather than `/data/difmsync.db`.
  The entrypoint refuses to start against an empty `/config` while a database
  is still mounted at the old path, rather than silently beginning with a
  fresh, unauthorized one.
- Configuration is environment variables throughout. Non-secret defaults moved
  into the image, and the `.env.defaults` / `.env.<env>` layering is gone.
  `.env.local` remains for a checkout, holding secrets only.

### Fixed

- The daemon now notices a refresh token stored by any other route while it is
  waiting for consent. Previously, authorizing through a sidecar or restoring a
  database left it waiting forever on a URL nobody was going to open, with the
  account already authorized.
- Restoring a backup no longer ends in a crash loop. `docker cp` lands the file
  owned by root and mode `0600`, which the service cannot read; the entrypoint
  now repairs the database and its sidecars by name rather than only the
  `/config` directory, which in steady state is already correct.
- Upgrading with `DIFMSYNC_DB_PATH=/data/difmsync.db` kept works. The old
  distroless volume is owned by uid 65532, and only `/config` was ever chowned.
- Day-2 `docker exec` commands no longer leave root-owned files behind. `backup`
  was the lasting one: run as root it created `/config/backups` and every
  snapshot in it owned by root, which the service then could not write.
- `auth --manual` no longer echoes the pasted authorization code back in its
  error messages.
