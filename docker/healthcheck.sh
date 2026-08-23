#!/bin/sh
# Container healthcheck.
#
# It goes through /difmsync, which drops privileges, and that is the whole
# reason this file exists rather than the healthcheck naming the binary
# directly. `docker exec` runs as the image USER, which is root here,
# while the daemon runs as PUID. Two ways that goes wrong, both observed:
#
#   - On a /config the daemon has not populated yet — first start, a
#     restored volume, a mount that came up late — a root-run check
#     creates the database itself, and it lands owned by root:root. The
#     daemon then cannot open its own database. With the drop, the same
#     situation fails loudly instead, with the volume-ownership
#     diagnostic, and leaves the volume untouched.
#   - In steady state SQLite removes difmsync.db-wal and -shm on a clean
#     close, so a root-run check usually gets away with it. One killed at
#     the healthcheck `timeout` mid-transaction does not, and leaves a
#     root-owned WAL behind. The entrypoint will not repair that: its
#     chown re-runs only when /config *itself* has the wrong owner, which
#     in steady state it does not.
#
# --check rather than plain `status`: `status` only fails when the
# account row is missing, so a sync broken for a week still reported
# healthy. --check requires a clean, non-dry pass within
# DIFMSYNC_STATUS_MAX_AGE. It is also deliberately not a curl of
# /healthz — internal/status is the single implementation of the health
# rule either way, and this still works when DIFMSYNC_HTTP_ADDR is unset.
set -eu

# Through /difmsync rather than repeating the drop, so there is one
# implementation of it and every exec-based entry point shares it.
exec /difmsync status --check
