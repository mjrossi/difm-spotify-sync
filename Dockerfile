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
#
# TestGoVersionPinsAgree enforces the three, because this comment claimed
# it and nothing checked. govulncheck runs on mise's Go, so a stale pin
# here reports "No vulnerabilities found" while every published image is
# built on the vulnerable toolchain — green scan, shipped CVEs.
FROM --platform=$BUILDPLATFORM golang:1.26.7-alpine AS build
WORKDIR /src

# Dependencies first so the module cache survives source-only changes.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

ARG TARGETARCH

# Stamped into the binary below as well as onto the image label, so
# `difmsync --version` and `docker inspect` agree. The release workflow
# passes the git tag; an untagged local build says "dev", which is true.
ARG VERSION=dev
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=$VERSION" \
    -o /out/difmsync ./cmd/difmsync

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
COPY docker/entrypoint.sh docker/healthcheck.sh docker/difmsync /
RUN chmod +x /entrypoint.sh /healthcheck.sh /difmsync

ARG VERSION=dev
LABEL org.opencontainers.image.title="difm-spotify-sync" \
      org.opencontainers.image.description="Mirrors DI.fm liked tracks into a Spotify playlist" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.source="https://github.com/mjrossi/difm-spotify-sync"

# /config, the convention self-hosters already have a mount for. The
# entrypoint chowns it to PUID:PGID on start, so a bind mount whose
# ownership does not match works without the operator pre-chowning it.
VOLUME ["/config"]

# Image-level defaults, so `docker run` with nothing but the five secrets
# is a working deployment and there is no dotenv file to copy.
#
# The rule for this block: it holds only values that DIFFER from the
# binary's own default. Restating a default here creates a second copy
# with nothing keeping it honest — log format, log level, interval and
# max-age were all duplicated exactly, so editing one and not the other
# gave an image whose documented behaviour was not its real behaviour.
# Everything below is a fact about running in a container:
#
#   DB_PATH          must be inside the volume, or the database lands in
#                    the ephemeral layer and is lost on the next `up` —
#                    silently, because SQLite creates a fresh one.
#   AUTH_BIND        must be 0.0.0.0: a published port forwards to eth0,
#                    not loopback, so a consent listener on 127.0.0.1 is
#                    unreachable from the host however it is published.
#   HTTP_ADDR        both endpoints are off by default in the binary; a
#   AUTH_HTTP_ADDR   container is the deployment that wants them on.
#   REDIRECT_URL     points at the consent port this image publishes,
#                    rather than the CLI's :8888.
#
# All are overridable, and all are wrong to override without a reason.
#
# BuildKit warns SecretsUsedInArgOrEnv for the two names containing AUTH.
# Both are addresses to listen on, not credentials, and the check matches
# on the name alone. There is nothing to fix here.
#
# TestConfigSurfaceIsDocumentedAndConsistent enforces both halves: that
# what is here matches the README table, and that what is absent falls
# through to the flag default the table also states.
ENV DIFMSYNC_DB_PATH=/config/difmsync.db \
    DIFMSYNC_AUTH_BIND=0.0.0.0 \
    DIFMSYNC_HTTP_ADDR=0.0.0.0:3436 \
    DIFMSYNC_AUTH_HTTP_ADDR=0.0.0.0:3437 \
    DIFMSYNC_SPOTIFY_REDIRECT_URL=http://127.0.0.1:3437/callback

# 3436 is "DIFM" on a phone keypad, chosen to stay clear of the ports a
# homelab usually has spoken for (3000, 8080/8081, 8096, 8123, 8384,
# 9000, 9090, 9100) and to sit below Linux's default ephemeral range,
# where a listener can collide with an outbound connection's source port.
EXPOSE 3436 3437

# In the image rather than only in compose.yaml, so the `docker run`
# deployment the README leads with is monitored too. Nothing external
# triggers a sync pass — the interval is an internal ticker — so an
# unhealthy container is the only signal that syncing has stopped.
#
# start-period is long because the first pass cannot succeed until
# consent is given, and that is a human step with no deadline. A shorter
# one would report a container unhealthy for no reason other than that
# nobody has opened the URL yet.
HEALTHCHECK --interval=5m --timeout=10s --start-period=30m --retries=3 \
    CMD /healthcheck.sh

# Root at entry, by design — see the entrypoint. It drops to PUID:PGID
# with `exec`, so difmsync still becomes pid 1 and receives SIGTERM
# directly.
ENTRYPOINT ["/entrypoint.sh"]
CMD ["sync", "--loop"]
