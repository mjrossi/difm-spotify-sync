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

# Validate before use, because all three reach a busybox builtin that
# reports the problem without naming the variable or this file. An
# invalid UMASK aborts at `umask` with "'b': invalid symbolic mode
# operator", which is loud but tells the operator nothing about which
# setting of theirs produced it.
case "$PUID" in
    '' | *[!0-9]*)
        log "PUID must be a number, got '$PUID'"
        exit 1
        ;;
esac
case "$PGID" in
    '' | *[!0-9]*)
        log "PGID must be a number, got '$PGID'"
        exit 1
        ;;
esac
case "$UMASK" in
    '' | *[!0-7]*)
        log "UMASK must be an octal mode such as 022, got '$UMASK'"
        exit 1
        ;;
esac

# Repair one path's ownership if it is wrong, quietly if it is already
# right or does not exist. Used below for the handful of paths the daemon
# must be able to write that the CONFIG_DIR chown does not reach.
repair_owner() {
    [ -e "$1" ] || return 0
    _owner="$(stat -c '%u:%g' "$1")"
    if [ "$_owner" != "${PUID}:${PGID}" ]; then
        log "chown $1 from ${_owner} to ${PUID}:${PGID}"
        chown "${PUID}:${PGID}" "$1"
    fi
}

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
# The limit that follows from it: this repairs the directory and the
# database below, not an arbitrary file some *other* root process created
# inside an already-correct directory. That is why /difmsync drops
# privileges itself rather than relying on the next restart to clean up
# after it.
owner="$(stat -c '%u:%g' "$CONFIG_DIR")"
if [ "$owner" != "${PUID}:${PGID}" ]; then
    log "chown ${CONFIG_DIR} from ${owner} to ${PUID}:${PGID}"
    chown -R "${PUID}:${PGID}" "$CONFIG_DIR"
fi

# The database, specifically, because it is the one file that arrives
# with the wrong owner inside a directory whose own owner is correct —
# which is exactly the case the chown above skips:
#
#   - A restore. `docker cp` chowns what it copies to the container's
#     user, which is root here, and `difmsync backup` wrote it 0600. The
#     daemon cannot even read the result.
#   - A root-run command killed mid-write, leaving root-owned -wal/-shm.
#   - A DIFMSYNC_DB_PATH pointed outside CONFIG_DIR, which the chown
#     above never reaches. The parent goes first for this case: SQLite
#     creates the sidecars next to the database, so the directory has to
#     be writable even when the file itself is already right.
#
# Four named paths rather than a recursive walk, so this stays cheap
# enough to run on every start with a year of backups in /config.
db="${DIFMSYNC_DB_PATH:-$CONFIG_DIR/difmsync.db}"
repair_owner "$(dirname "$db")"
repair_owner "$db"
repair_owner "$db-wal"
repair_owner "$db-shm"

log "starting as uid ${PUID} gid ${PGID}, umask ${UMASK}, TZ ${TZ:-UTC}, db ${DIFMSYNC_DB_PATH:-$CONFIG_DIR/difmsync.db}"

# exec, so difmsync becomes pid 1 and receives SIGTERM directly. A shell
# left in the middle would swallow it, and the engine's clean-stop path —
# finish the pass in flight, leave the watermark where it was — depends
# on that signal arriving. stop_grace_period is 45s for exactly this.
exec su-exec "${PUID}:${PGID}" /app/difmsync "$@"
