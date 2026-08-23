# syntax=docker/dockerfile:1
# Declared because this file uses `RUN --mount=type=cache`, which the
# legacy (non-BuildKit) builder rejects outright. BuildKit is also what
# provides TARGETARCH below, so a `DOCKER_BUILDKIT=0` build fails
# immediately rather than producing a subtly wrong image.

# The build stage stays on the *build* platform and cross-compiles, so an
# arm64 image is produced at native speed rather than through QEMU. This
# is only possible because CGO_ENABLED=0 holds — modernc.org/sqlite is
# pure Go — which is the same property that makes the binary static.
#
# Patch-pinned to match mise.toml and go.mod. A floating `1.26-alpine`
# would build the release artifact on a different toolchain than
# `just check` verified.
FROM --platform=$BUILDPLATFORM golang:1.26.5-alpine AS build
WORKDIR /src

# Dependencies first so the module cache survives source-only changes.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

ARG TARGETARCH
ARG VERSION=dev
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/difmsync ./cmd/difmsync

# Alpine rather than distroless, deliberately, and it is a trade rather
# than an upgrade.
#
# Distroless gave a smaller attack surface and a process that was never
# root. What it could not give is the PUID/PGID contract every
# self-hosted image is expected to honour, because applying it needs a
# shell and a chown before dropping privileges. The failure that costs
# more in practice is the one PUID/PGID fixes: a bind-mounted config
# directory whose ownership does not match the image's fixed uid, which
# SQLite reports as "unable to open database file" and `restart:
# unless-stopped` turns into a silent crash loop.
#
# The window where this image is root is the few milliseconds in
# docker/entrypoint.sh before `exec su-exec`. See that file.
FROM alpine:3.22
WORKDIR /app

# su-exec is the privilege drop (a ~10KB C program; gosu is a ~2MB Go
# one that does the same job). tzdata is what makes TZ mean anything —
# without it every timestamp an operator reads is UTC regardless of what
# they set. ca-certificates is needed to talk to Spotify and DI.fm at
# all; alpine does not ship it.
RUN apk add --no-cache su-exec tzdata ca-certificates

COPY --from=build /out/difmsync /app/difmsync
COPY docker/entrypoint.sh docker/healthcheck.sh /
RUN chmod +x /entrypoint.sh /healthcheck.sh

ARG VERSION=dev
LABEL org.opencontainers.image.title="difm-spotify-sync" \
      org.opencontainers.image.description="Mirrors DI.fm liked tracks into a Spotify playlist" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.source="https://github.com/mjrossi/difm-spotify-sync"

# /config, not /data: the convention self-hosters already have a mount
# for. The entrypoint refuses to start against an empty /config while a
# database is still mounted at the old /data path, so upgrading cannot
# silently begin with a fresh, unauthorized database.
VOLUME ["/config"]

# Image-level defaults. These live here rather than in a dotenv file the
# operator has to copy, which is what makes `docker run` with nothing but
# the five secrets a working deployment.
#
# Two of them are facts about running in a container rather than
# preferences. DIFMSYNC_DB_PATH must be inside the volume or the database
# lands in the container's ephemeral filesystem, where it is lost on the
# next `up` — silently, because SQLite will happily create a fresh one.
# DIFMSYNC_AUTH_BIND must be 0.0.0.0 because a published port forwards to
# the container's eth0 address, not its loopback, so a consent listener
# on 127.0.0.1 is unreachable from the host no matter how it is
# published. Both are still overridable; both are wrong to override
# without a specific reason.
# BuildKit warns SecretsUsedInArgOrEnv for the two names containing
# AUTH. Both are addresses to listen on, not credentials, and the check
# matches on the name alone. There is nothing to fix here.
ENV DIFMSYNC_DB_PATH=/config/difmsync.db \
    DIFMSYNC_AUTH_BIND=0.0.0.0 \
    DIFMSYNC_LOG_FORMAT=json \
    DIFMSYNC_LOG_LEVEL=info \
    DIFMSYNC_INTERVAL=15m \
    DIFMSYNC_HTTP_ADDR=0.0.0.0:3436 \
    DIFMSYNC_AUTH_HTTP_ADDR=0.0.0.0:3437 \
    DIFMSYNC_SPOTIFY_REDIRECT_URL=http://127.0.0.1:3437/callback \
    DIFMSYNC_STATUS_MAX_AGE=45m

# 3436 is "DIFM" on a phone keypad, chosen to stay clear of the ports a
# homelab usually has spoken for (3000, 8080/8081, 8096, 8123, 8384,
# 9000, 9090, 9100) and to sit below Linux's default ephemeral range,
# where a listener can collide with an outbound connection's source port.
EXPOSE 3436 3437

# Root at entry, by design — see the entrypoint. It drops to PUID:PGID
# with `exec`, so difmsync still becomes pid 1 and receives SIGTERM
# directly.
ENTRYPOINT ["/entrypoint.sh"]
CMD ["sync", "--loop"]
