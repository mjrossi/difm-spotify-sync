# Security

## Reporting a vulnerability

Report privately through GitHub's [private vulnerability
reporting](https://github.com/mjrossi/difm-spotify-sync/security/advisories/new)
rather than opening a public issue. Include what you did, what happened, and
what you expected — a proof of concept is welcome but not required.

This is a hobby project maintained by one person; expect a reply within a week
or so, not within hours. There is no bounty.

## What this software holds

Three secrets, all of them yours:

- **A Spotify refresh token**, in the SQLite database. It grants unattended
  playlist write access to your account until you revoke it in Spotify's
  [apps settings](https://www.spotify.com/account/apps/). The database file is
  created `0600` before anything is written to it, and `difmsync backup`
  snapshots are too.
- **`DIFMSYNC_API_KEY`**, a long-lived DI.fm token. DI.fm exposes no rotation
  path for it, so treat it as shared-fate: if it leaks, the remedy is
  contacting DI.fm, not rotating a key. It is never logged, never written to
  `docs/`, and `pkg/difm` scrubs the member id out of error text — DI.fm
  request URLs carry it as a path segment, and `*url.Error` embeds the URL.
- **Your Spotify client secret**, supplied through the environment.

If you are restoring or copying a database between hosts, remember it carries
the refresh token.

## Threat model of the HTTP endpoints

The daemon serves two read-only endpoints — `GET /healthz` and
`GET /status.json` — without authentication, and `compose.yaml` publishes them
to the LAN. That is defensible only because of two properties that are enforced
by tests rather than by convention:

- **They cannot change anything.** Everything that writes to Spotify or to the
  database is a CLI action.
- **They carry no secrets.** The report is assembled field by field from typed
  accessors rather than by serializing a store struct — the accounts row holds
  the refresh token on the same row as the playlist label. Recorded error text
  is excluded from both endpoints as well, because it is assembled from
  whatever failed and reviewed by nobody.

Do not add a write endpoint there. If you need one, it needs its own listener
and its own guard, the way the consent server has.

## The consent server

While there is no refresh token, the daemon serves a one-time Spotify consent
flow on a separate port. It writes a refresh token, so it is deliberately not
part of the status server, and it is narrow on purpose:

- It exists only until consent is stored, then shuts down for the life of the
  process.
- Starting a flow requires a nonce generated at startup and emitted once, to
  the log. Reaching the port is not sufficient — without this, anyone who could
  reach it could complete consent with *their* Spotify account and bind your
  sync to a stranger's playlist.
- The callback itself is guarded by the OAuth `state` parameter, since Spotify
  redirects a browser to it and will not carry an extra parameter.

`compose.yaml` publishes that port to loopback only. If you put a proxy in
front of it — `tailscale serve`, for instance — the audience becomes whatever
that proxy serves, and the nonce is then the only thing standing in front of
the flow. That is why it is a nonce and not a convenience.

## Supported versions

The latest release. This project has no long-term support branches.
