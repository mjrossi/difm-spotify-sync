# Data hygiene: prune sync_runs, refuse a corrupt database, fuzz the matcher

Date: 2026-09-22. Status: approved design, awaiting implementation plan.
Slice 3 of four; slices 1–2 are `2026-09-17-engine-resilience-design.md`
and `2026-09-21-operator-diagnostics-design.md`.

## Context

Three things that do not fail today but will, quietly:

- `sync_runs` grows one row per tick forever — ~35,000 a year at the
  default 15m. Nothing reads past the newest 20, every backup carries
  all of them, and the table has no retention rule at all.
- A corrupt or half-restored database surfaces as whatever the driver
  says first — `SQL logic error`, `database disk image is malformed` —
  from deep inside a query, and `restart: unless-stopped` turns that
  into a crash loop with a message that names neither the file nor the
  fix. The documented restore path (`docker cp` a backup in) is exactly
  where a truncated copy arrives.
- `pkg/match` normalizes and scores whatever DI.fm and Spotify send.
  Both are decoded defensively, but the matcher has never been fed
  arbitrary input; a panic on an odd title takes the daemon down on
  every tick that re-reads the same like.

## Decisions

1. **Retention is 90 days, never fewer than the newest 20 rows, pruned
   after each clean pass.** No flag: a retention period is not a knob a
   self-hoster needs, and every flag is a README row and a Dockerfile
   line the config-drift test then polices. The floor of 20 is the
   health scan window (`status.HealthScanLimit`) and is asserted equal
   to it by a test, so the health rule can never lose a row to pruning.
2. **A failed `quick_check` refuses to open.** The error names the file
   and the restore section of the runbook. Under a restart policy that
   is a loud loop with a readable reason, which is the best a corrupt
   database can offer; opening anyway and writing on top of it is not.
3. **Fuzz targets for `Normalize`, `Parse` and `Score`**, seeded from
   the existing table tests, run as seed-only in `just check` and for a
   bounded time under a new `just fuzz` recipe. The properties are: no
   panic, `Normalize` is idempotent, and a score lies in `[0, 1]`.

## Section 1: Pruning

`internal/store/sqlite/queries/sync_runs.sql` gains:

```sql
-- name: PruneSyncRuns :execrows
DELETE FROM sync_runs
WHERE account_id = ?
  AND finished_at IS NOT NULL
  AND started_at < ?
  AND id NOT IN (
    SELECT id FROM sync_runs
    WHERE account_id = ?
    ORDER BY started_at DESC, id DESC
    LIMIT ?
  );
```

The in-flight guard (`finished_at IS NOT NULL`) means the row the
current pass opened can never be deleted by its own prune, and the
`NOT IN` floor means the newest `keep` rows survive regardless of age.
The final token is a bound parameter (CLAUDE.md, sqlc rule).

`Store.PruneRuns(ctx, accountID, before time.Time, keep int) (int64, error)`
wraps it. The engine calls it at the end of a clean, non-dry pass — after
the ledger transaction, before `sync complete` — with
`before = now - RunsRetention` and `keep = KeepRuns`, both exported
constants in `internal/syncer` (`90 * 24 * time.Hour` and `20`). A prune
error is logged at Warn and does not touch `passClean`: housekeeping is
not a like reaching durable state, and invariant 2 is about likes. A
non-zero deletion count is logged at Debug.

`loop_test.go` (package `syncer`) asserts `KeepRuns ==
status.HealthScanLimit`; `status` does not import `syncer`, so the
test-only import is not a cycle.

## Section 2: quick_check

`sqlite.Open`, after the ping, runs `PRAGMA quick_check` and requires
the single result row to be `ok`. Anything else closes the handle and
returns:

```
sqlite.Open: <path> failed integrity check (<first line of the result>);
restore from a backup — docs/deploy.md, Restoring
```

`quick_check` skips the index-consistency pass that `integrity_check`
does, so it is cheap enough to run on every open — including the
healthcheck's `status --check` every five minutes and `backup`'s
verification of its own snapshot. The error text carries the path and
SQLite's own one-line diagnosis, which names no secret.

The test writes a valid migrated database, overwrites a page in the
middle of the file with garbage, and asserts `Open` fails with a message
containing "integrity check" and "Restoring". The negative control is
every other test in the package: a healthy database still opens.

## Section 3: Fuzzing

`pkg/match/fuzz_test.go`:

- `FuzzNormalize(s)`: `Normalize` does not panic; `Normalize(Normalize(s)) == Normalize(s)`.
- `FuzzParse(artist, title)`: `Parse` does not panic; every artist and
  the title it returns are already normalized (`Normalize(x) == x`).
- `FuzzScore(a1, t1, d1, a2, t2, d2)`: `Score(Parse(a1,t1), d1, Parse(a2,t2), d2)`
  does not panic and `0 <= Score <= 1`; a track scored against itself
  with equal durations scores 1 when its title normalizes to something
  non-empty.

Seeds: the artist/title pairs from `TestParse`, `TestScore_EditsDoNotCollide`
and `TestScore_UnnormalizableTitlesDoNotMatch`, plus a handful of
adversarial strings (empty, whitespace only, a lone `(`, mixed scripts,
a 10 kB title, a title of only feat./remix markers).

`justfile` gains `fuzz`: `go test ./pkg/match -run '^$' -fuzz Fuzz… -fuzztime 20s`
for each target in turn. `just check` is unchanged — `go test ./...`
already executes the seed corpus of every `Fuzz*` function.

## Tests

- Store: `PruneRuns` deletes only rows older than `before` beyond the
  newest `keep`; never an in-flight row; returns the count; a second
  call deletes nothing.
- Engine: a clean pass prunes (harness with the store clock rewound to
  seed 25 old rows, 5 survive plus the new one); a failed pass does not
  prune; a dry run does not prune; a prune error does not mark the pass
  incomplete.
- `KeepRuns == status.HealthScanLimit`.
- `Open` on a corrupted file fails with the restore message.
- Fuzz seeds pass under `go test ./pkg/match`.

## Documentation

CLAUDE.md: Sync semantics gains the retention rule and the reason it is
not a flag; Testing notes `just fuzz` and that seeds run in the gate.
`docs/deploy.md`: Backups section notes that a snapshot carries at most
90 days of run history; Restoring section says a corrupt file is refused
at start with a message pointing here. CHANGELOG `[Unreleased]`.

## Out of scope

Pruning `review_queue` (resolved items are the operator's audit trail);
`integrity_check`; anything in slice 4.
