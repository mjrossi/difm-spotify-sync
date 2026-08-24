# difm-spotify-sync — common dev commands.
# Run `just` (no args) to list recipes, organized by group.
#
# `just` itself is pinned in mise.toml (`aqua:casey/just`); a single
# `mise install` at the repo root provisions it alongside go, sqlc,
# goose, and golangci-lint.

set shell := ["bash", "-cu"]

# Recipes call tools through `mise exec --` rather than bare names, so a
# contributor who ran `mise install` but has not shell-activated mise
# still gets the pinned versions — and the DIFMSYNC_* defaults from
# mise.toml's [env], which the run recipes depend on.

# ── default ───────────────────────────────────────────

[private]
default:
    @just --list --unsorted

# ── build & verify ────────────────────────────────────

# format Go code (gofumpt + goimports, configured in .golangci.yml)
[group('build')]
fmt:
    mise exec -- golangci-lint fmt

# `just` uses only the LAST comment line as a recipe's description, so
# this one carries an attribute instead of a multi-line comment.
[doc('golangci-lint v2 — the single lint + format gate')]
[group('build')]
lint:
    mise exec -- golangci-lint run

# go test ./... with race detector, no cache (matches CI)
[group('build')]
test:
    mise exec -- go test ./... -race -count=1

# regenerate sqlc bindings from migrations-sqlite/ + queries/
[group('build')]
gen:
    mise exec -- sqlc generate

# fail if generated code is stale relative to the schema and queries
[group('build')]
gen-check: gen
    git diff --exit-code -- internal/store/sqlite/gen

# the full CI gate — run locally before pushing
[group('build')]
check: lint test gen-check tidy-check verify-config

# Four of fifteen variables had drifted between the code and the files
# that set them — a dead DIFMSYNC_NETWORK in mise.toml, a DIFM_* prefix in
# the docs, a redirect URL missing from the example. That ratio does not
# improve by hand.
#
# Both directions are checked. An orphan (set, read by nothing) is dead
# config; an undocumented variable (read, in no README table row) is a
# knob nobody can discover. A gate that only looks one way passes while
# half the drift it exists to catch walks straight through.
[group('build')]
[doc('check DIFMSYNC_* variables agree across code, config and docs')]
verify-config:
    #!/usr/bin/env bash
    set -euo pipefail
    # Tests are excluded: a variable named only in a test is not read by
    # the binary, and counting it would satisfy the orphan check falsely.
    code=$(grep -oh 'DIFMSYNC_[A-Z_]\+' \
             $(find cmd/difmsync -name '*.go' -not -name '*_test.go') | sort -u)
    refs=$(grep -roh 'DIFMSYNC_[A-Z_]\+' \
             mise.toml mise.development.toml mise.ci.toml \
             .env.local.example compose.yaml Dockerfile docker/ justfile \
             README.md docs/ .github/workflows/ \
           | sort -u)
    # The README table is the documented surface, so it is compared on
    # its own rather than folded into the union above — otherwise a
    # variable set in mise.toml and absent from the table passes.
    documented=$(grep -oh 'DIFMSYNC_[A-Z_]\+' README.md | sort -u)

    status=0
    orphans=$(comm -13 <(echo "$code") <(echo "$refs"))
    if [ -n "$orphans" ]; then
        echo "set somewhere but read by nothing in cmd/difmsync:" >&2
        echo "$orphans" >&2
        status=1
    fi
    undocumented=$(comm -23 <(echo "$code") <(echo "$documented"))
    if [ -n "$undocumented" ]; then
        echo "read by cmd/difmsync but missing from the README table:" >&2
        echo "$undocumented" >&2
        status=1
    fi
    [ "$status" -eq 0 ] || exit 1
    echo "config consistent: $(echo "$code" | wc -l | tr -d ' ') variable(s), no orphans, all documented"

# Mirrors gen-check: regenerate, then fail if the tree moved. `tidy` on its
# own enforced nothing — it just ran the command — so the actual gate lived
# inline in ci.yml, where a contributor running `just check` never saw it. A
# stale go.sum breaks the container build rather than the Go build, which is
# the kind of failure that should not wait for CI to find.
[group('build')]
[doc('fail if go.mod or go.sum is stale')]
tidy-check:
    mise exec -- go mod tidy
    git diff --exit-code -- go.mod go.sum

# ── run ───────────────────────────────────────────────

# one sync pass, writing nothing — the intended first run
[group('run')]
dry-run:
    mise exec -- go run ./cmd/difmsync sync --dry-run --log-format=text

# one sync pass, writing to the configured playlist
[group('run')]
sync:
    mise exec -- go run ./cmd/difmsync sync --log-format=text

# run continuously on DIFMSYNC_INTERVAL
[group('run')]
loop:
    mise exec -- go run ./cmd/difmsync sync --loop --log-format=text

# one-time Spotify OAuth consent
[group('run')]
auth:
    mise exec -- go run ./cmd/difmsync auth --log-format=text

# Paste-the-URL consent. Nothing listens, so the redirect URI only has to
# be registered with Spotify, not reachable — which is what makes this the
# fallback when the browser is not on the machine running the sync.
[group('run')]
[doc('one-time Spotify consent without a callback listener')]
auth-manual:
    mise exec -- go run ./cmd/difmsync auth --manual --log-format=text

# list the review queue
[group('run')]
review:
    mise exec -- go run ./cmd/difmsync review

# ledger totals and watermark
[group('run')]
status:
    mise exec -- go run ./cmd/difmsync status

# ── ops ───────────────────────────────────────────────

# Migrations are applied by the binary at boot through goose's library,
# not this CLI — but the CLI is the way to inspect what actually ran on a
# database, which is the first question when a deploy misbehaves.
[group('ops')]
[doc('show which migrations have been applied to the local database')]
migrate-status:
    @mise exec -- goose -dir migrations-sqlite sqlite3 "${DIFMSYNC_DB_PATH:-./tmp/difmsync.db}" status

[group('ops')]
[doc('list synced tracks with their DI.fm ids (needed by resync-track)')]
ledger:
    @mise exec -- sqlite3 -header -column "${DIFMSYNC_DB_PATH:-./tmp/difmsync.db}" \
        "select difm_track_id, artist, substr(title,1,40) as title, \
                round(match_score,3) as score, substr(added_at,1,10) as added \
         from synced_tracks order by liked_at desc;"

# Clears the track's ledger row AND rewinds the watermark. Both suppress a
# re-add, and clearing only the ledger is a silent no-op — the watermark
# filters at fetch time, so the like is never retrieved. `--forget` does
# both on its own; `--all` is deliberately NOT passed, because it would
# clear the watermark instead of rewinding it and re-read all of history.
# Find ids with `just ledger`, then `just sync` to actually re-add.
# Example: just resync-track 3057639
[group('ops')]
[doc('re-add ONE track you deleted from Spotify (takes a DI.fm track id)')]
resync-track ID:
    mise exec -- go run ./cmd/difmsync resync --forget={{ID}}

# Nothing already synced is forgotten, so this adds nothing that is already
# in the playlist. Useful after changing matching thresholds, or to pick up
# likes that predate the current watermark.
[group('ops')]
[doc('clear the watermark so the next sync re-reads the full like history')]
resync-all:
    mise exec -- go run ./cmd/difmsync resync --all

# For recovering from a restored or lost database. Safe: each pass reconciles
# against the playlist's live contents before adding, so tracks already there
# are recorded rather than duplicated.
[group('ops')]
[doc('drop the whole ledger and rebuild it from the playlist on next sync')]
resync-rebuild:
    mise exec -- go run ./cmd/difmsync resync --forget-all

# `difmsync backup` rather than `sqlite3 .backup`: it is the same
# VACUUM INTO under the hood, but it also runs inside the container,
# which ships no sqlite3. One code path for the local database and the
# deployed one beats two that can drift.
[group('ops')]
[doc('take a consistent backup of the database (holds the Spotify refresh token)')]
backup DEST="":
    #!/usr/bin/env bash
    set -euo pipefail
    # Timestamped by default. A fixed name worked exactly once: the command
    # refuses to overwrite an existing snapshot (it may be the only copy of
    # a refresh token), so a constant default turned every run after the
    # first into an error.
    dest='{{DEST}}'
    [ -n "$dest" ] || dest="./tmp/difmsync-backup-$(date +%Y%m%d-%H%M%S).db"
    mise exec -- go run ./cmd/difmsync backup --to "$dest" --log-format=text

# open the local SQLite database
[group('ops')]
db:
    mise exec -- sqlite3 "${DIFMSYNC_DB_PATH:-./tmp/difmsync.db}"
