# Built-in backups and a narrowed container

Date: 2026-09-24. Status: approved design, awaiting implementation plan.
Slice 4 of four, the last.

## Context

Two gaps that the runbook currently papers over with instructions:

- **Backups are manual.** `difmsync backup --to=` exists, and
  `docs/deploy.md` tells the operator to wire a host cron and a `find
  -mtime +14 -delete`. The `docker run` deployment the README leads with
  therefore has no backups at all unless someone reads that far — and
  the database is the only copy of the Spotify refresh token. Worse, the
  documented cron runs `docker compose exec`, which runs **as root**, so
  every snapshot and the `/config/backups` directory it creates are
  root-owned: the exact failure CLAUDE.md's Deployment section says
  lasts longest.
- **The container keeps every default capability.** `compose.yaml` sets
  memory and CPU limits and a log cap, but no `cap_drop`, no
  `no-new-privileges`. The image deliberately starts as root to honour
  `PUID`/`PGID` and then `exec su-exec`s down, which is a defensible
  trade — but it means the root window should be as narrow as the
  entrypoint's actual needs, not as wide as Docker's defaults.

## Decisions

1. **The daemon takes its own daily snapshot**, after the first clean
   pass of each UTC day, keeping `--backup-keep` (14) files in
   `--backup-dir`. It runs as the daemon's own uid, so nothing it writes
   is root-owned — which is the half of the problem the host cron cannot
   fix. The image defaults `--backup-dir` to `/config/backups`; the CLI
   default is empty, so a workstation run backs nothing up by surprise.
2. **`compose.yaml` drops all capabilities and adds back exactly what
   the entrypoint needs**, plus `no-new-privileges`. The set is proven
   by a new assertion in `container-tests.yml` rather than reasoned
   about, because a missing capability fails at `chown` in a way that
   `restart: unless-stopped` turns into a loop. Read-only rootfs is
   explicitly out of scope: it needs its own tmpfs list and its own
   assertion, and the return over `cap_drop` is small.

## Section 1: Scheduled backups

Two flags (`cmd/difmsync/main.go`, on `sync`):

- `--backup-dir` / `DIFMSYNC_BACKUP_DIR` — empty disables; the
  `Dockerfile` `ENV` sets `/config/backups`, so it is on for the image
  and off for a bare binary. A README row and a Dockerfile line follow,
  which `TestConfigSurfaceIsDocumentedAndConsistent` then polices.
- `--backup-keep` / `DIFMSYNC_BACKUP_KEEP` — default 14, `0` keeps all.

`internal/syncer` gains a `Backups` field on `Engine`:

```go
// Backups, when set, takes one snapshot per UTC day after a clean pass.
type Backups struct {
	Dir  string
	Keep int
}
```

`RunOnce` calls it from the same guarded block as the prune — after the
ledger commit, `passClean && !dryRun && ctx.Err() == nil` — and before
the prune, so a snapshot is never taken of a database whose retention
has just changed underneath it. The steps, in `internal/syncer/backup.go`:

1. Derive `difmsync-YYYY-MM-DD.db` from `time.Now().UTC()`. If that file
   already exists, today's snapshot is done: return without work. This
   is what makes "daily" cheap to check and idempotent across restarts —
   no state beyond the directory listing.
2. `MkdirAll(Dir, 0o700)`, then `store.BackupTo(ctx, path)` — the
   existing `VACUUM INTO` + verify + rename that `difmsync backup`
   already uses. **Reuse it; do not write a second snapshot path.**
3. Prune: list `difmsync-*.db`, sort by name (ISO dates sort
   lexically), delete all but the newest `Keep`. `Keep <= 0` skips.

Every failure is a Warn and leaves `passClean` alone, for the same
reason the run prune does: a snapshot is not a like reaching durable
state. But unlike the prune, a *persistent* backup failure is worth
surfacing — a disk that filled is exactly the silent failure this
project keeps legislating against. So the daemon logs at Warn each
time, and `status` reports it: `Report` gains `last_backup_at`
(the newest snapshot's date, from the directory, not from a new store
column) and `/healthz` is **unaffected** — a missing backup does not
make the sync unhealthy, because it does not mean syncing stopped.

`docs/deploy.md` loses the host-cron recipe and gains "it happens on its
own; here is where the files are and how to restore one".

## Section 2: Capabilities

`compose.yaml`:

```yaml
    security_opt:
      - no-new-privileges:true
    cap_drop:
      - ALL
    cap_add:
      - CHOWN           # entrypoint repairs /config and the database
      - DAC_OVERRIDE    # ...including files a foreign uid owns
      - FOWNER          # chown of a file whose uid differs from ours
      - SETUID          # su-exec drops to PUID
      - SETGID          # ...and PGID
```

Each line carries the entrypoint step that needs it, because the next
person to trim the list needs to know what breaks. The set is a
hypothesis until CI proves it: `container-tests.yml` gains a job that
runs the image with exactly this set against a bind mount owned by a
*different* uid, asserts the database ends up owned by `PUID`, and —
the negative control CLAUDE.md's Testing section requires — runs the
same case with `CHOWN` removed and asserts it fails. Without that
control the test passes whether or not the capability is needed.

`no-new-privileges` is safe here because `su-exec` drops privileges
rather than gaining them; the flag blocks the setuid-escalation path
only.

The `docker run` line in the README gains the same flags, since that is
the deployment the README leads with.

## Tests

- `internal/syncer/backup_test.go`: a clean pass writes today's file;
  a second pass the same day writes nothing new; `Keep=2` leaves the
  two newest of five; `Keep=0` leaves all; a failure (unwritable dir)
  is a Warn and the pass still succeeds; a dry run writes nothing.
- The snapshot is a real database: open it and read the account row
  (`BackupTo` already verifies, so assert the file opens, not the bytes).
- `status`: `last_backup_at` is the newest file's date; absent when the
  directory is empty or unset; `/healthz` verdict unchanged either way.
- Config surface: the two new variables appear in the README table and
  the Dockerfile `ENV` block with matching defaults.
- Container: the capability job and its negative control.

## Documentation

README configuration table and the `docker run` example; `docs/deploy.md`
Backups section rewritten; CLAUDE.md Deployment gains the capability set
and why each is there, and Sync semantics notes the snapshot shares the
prune's guard; CHANGELOG.

## Out of scope

Read-only rootfs; off-host backup shipping; restic/borg integration;
encrypting the snapshot (it holds a refresh token, but so does the live
database next to it, and the volume is the security boundary either way).
