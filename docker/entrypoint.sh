#!/bin/sh
# Container entrypoint. Applies the PUID/PGID/UMASK contract that
# self-hosted images are expected to honour, then drops to that user and
# runs difmsync.
#
# Why this exists at all: the database is bind-mounted as often as not,
# and a bind mount arrives with the ownership the host directory already
# has. An image that runs as a fixed uid can only work if the host
# happens to agree with it, and when it does not the failure is a crash
# loop — SQLite reports "unable to open database file" and the restart
# policy hides it. PUID/PGID move that decision to the operator, who is
# the only one who knows what the host directory looks like.
#
# The cost is real and worth stating: this runs as root for the few
# milliseconds before su-exec, which the previous distroless image never
# did. Everything below is written to keep that window doing as little as
# possible.
set -eu

PUID="${PUID:-1000}"
PGID="${PGID:-1000}"
UMASK="${UMASK:-022}"
CONFIG_DIR="${CONFIG_DIR:-/config}"

log() { printf '%s difmsync-init: %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$*" >&2; }

# The /data -> /config migration, caught rather than papered over.
#
# Deployments before this change mounted the database at /data. If that
# volume is still mounted and the configured path is empty, starting
# would create a *fresh* database beside the real one: a daemon with no
# account row, no ledger and no refresh token, reporting itself as a
# clean first run. Nothing about that looks like a mistake until the
# Spotify consent prompt reappears for a deployment that was authorized
# months ago.
if [ -f /data/difmsync.db ] && [ ! -f "${DIFMSYNC_DB_PATH:-$CONFIG_DIR/difmsync.db}" ]; then
    log "found a database at /data/difmsync.db, but DIFMSYNC_DB_PATH points at ${DIFMSYNC_DB_PATH:-$CONFIG_DIR/difmsync.db}, which does not exist."
    log "This image mounts its database at ${CONFIG_DIR} instead of /data. Refusing to start"
    log "with a fresh database while the old one is still mounted, because that looks"
    log "like a working first run and is not one — the refresh token and the whole sync"
    log "ledger are in the file at /data."
    log ""
    log "Either remap the volume:      - difmsync-data:${CONFIG_DIR}"
    log "or keep the old path:         -e DIFMSYNC_DB_PATH=/data/difmsync.db"
    exit 1
fi

umask "$UMASK"

# Already unprivileged: the operator set `user:` in compose, or docker
# run --user. Nothing here can chown anything, and trying would fail the
# `set -e` above. Their choice of uid is the whole instruction.
if [ "$(id -u)" != "0" ]; then
    log "running as uid $(id -u) gid $(id -g) (PUID/PGID ignored; the container was started with an explicit user), umask ${UMASK}"
    exec /app/difmsync "$@"
fi

mkdir -p "$CONFIG_DIR"

# Non-recursive unless the top level is actually wrong. `chown -R` over a
# config directory holding a year of nightly backups adds seconds to
# every restart to fix something that is almost never broken.
#
# The limit that follows from it: this repairs the directory, not a file
# some *other* root process created inside an already-correct one. That
# is why docker/healthcheck.sh drops privileges itself rather than
# relying on the next restart to clean up after it.
owner="$(stat -c '%u:%g' "$CONFIG_DIR")"
if [ "$owner" != "${PUID}:${PGID}" ]; then
    log "chown ${CONFIG_DIR} from ${owner} to ${PUID}:${PGID}"
    chown -R "${PUID}:${PGID}" "$CONFIG_DIR"
fi

log "starting as uid ${PUID} gid ${PGID}, umask ${UMASK}, TZ ${TZ:-UTC}, db ${DIFMSYNC_DB_PATH:-$CONFIG_DIR/difmsync.db}"

# exec, so difmsync becomes pid 1 and receives SIGTERM directly. A shell
# left in the middle would swallow it, and the engine's clean-stop path —
# finish the pass in flight, leave the watermark where it was — depends
# on that signal arriving. stop_grace_period is 45s for exactly this.
exec su-exec "${PUID}:${PGID}" /app/difmsync "$@"
