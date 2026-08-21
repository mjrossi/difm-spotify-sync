# syntax=docker/dockerfile:1
# Declared because this file uses `RUN --mount=type=cache`, which the
# legacy (non-BuildKit) builder rejects outright. GitHub runners default
# to BuildKit; a homelab `DOCKER_BUILDKIT=0 docker compose build` does
# not, and this is the only builder that matters now that the homelab is
# the sole deployment target.
# Multi-stage build. The final image is distroless and non-root; the
# binary is fully static (modernc.org/sqlite is pure Go, so no CGO and no
# libc dependency), so the runtime stage needs no libc and no shell.

# Patch-pinned to match mise.toml and go.mod. A floating `1.26-alpine`
# would build the release artifact on a different toolchain than `just
# check` verified, which is exactly the drift the pinning policy exists
# to prevent.
FROM golang:1.26.5-alpine AS build
WORKDIR /src

# Dependencies first so the module cache survives source-only changes.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/difmsync ./cmd/difmsync

# Staged here so the directory can be copied into the final image with
# the right ownership. distroless has no shell, so there is no way to
# mkdir or chown once we are there.
RUN mkdir -p /data

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/difmsync /app/difmsync

# Mounted volume for the SQLite database.
#
# The ownership matters and is easy to lose: Docker initializes a named
# volume from the image's directory *including its uid/gid*, so a
# root-owned /data in the image yields a root-owned volume that the
# nonroot process cannot write. The failure surfaces as an opaque
# "unable to open database file" and, with restart: unless-stopped, an
# endless crash loop. 65532 is distroless's nonroot uid, written
# numerically because --chown resolves names against the *build* stage.
COPY --from=build --chown=65532:65532 /data /data
VOLUME ["/data"]
ENV DIFMSYNC_DB_PATH=/data/difmsync.db

USER nonroot:nonroot
ENTRYPOINT ["/app/difmsync"]
CMD ["sync", "--loop"]
