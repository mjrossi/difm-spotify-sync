# Operator diagnostics: max-age follows the interval, one line per idle pass, a fuller status report

Date: 2026-09-21. Status: approved design, awaiting implementation plan.
Slice 2 of four; slice 1 is `2026-09-17-engine-resilience-design.md`.

## Context

Three traps for the operator, none of which loses data:

- `--max-age` (`DIFMSYNC_STATUS_MAX_AGE`) defaults to a fixed 45m
  regardless of `--interval`. An operator who raises the interval past
  ~20m and does not know to raise `max-age` too gets a healthcheck that
  is red between every pair of passes, and `restart: unless-stopped`
  ignores health, so nothing visibly breaks — the probe is simply wrong
  from then on. The maintainer's own 2h deployment needed the variable
  set by hand.
- An idle pass (no new likes) logs two Info lines that say nothing —
  `fetched likes count=0` and `sync complete added=0 queued=0 skipped=0`
  — and neither says when the next pass is. Twelve times a day at a 2h
  interval, the log is noise that still fails to answer "is it alive and
  when does it try again".
- `/status.json` reports `healthy` but not *when* the last success was,
  how many failures have stacked since, or which binary is answering —
  the three numbers a probe or a human wants next to the boolean.

## Decisions

1. **`max-age` defaults to 3 × interval when not set explicitly.** The
   flag's declared default stays `45m` — which *is* 3 × the 15m default
   interval, so the README table, the Dockerfile and the config-drift
   test are all untouched and existing deployments see no change. The
   derivation applies only when `DIFMSYNC_STATUS_MAX_AGE`/`--max-age`
   is absent (`c.IsSet`); an explicit value always wins. `status` gains
   an env-backed `--interval` (same variable, same 15m default as
   `sync`'s) so `status --check` in the container derives the same
   number the daemon does.
2. **One Info line per pass, emitted by `Loop`, carrying `next_run`.**
   `RunOnce` keeps its detail lines but at Info only when they carry
   information: `fetched likes` when the count is non-zero, `sync
   complete` when the pass fetched anything. Both drop to Debug
   otherwise. `Loop` logs `pass finished` at Info after every pass with
   the counts, whether it was clean, and `next_run` as a timestamp. An
   idle loop pass is therefore exactly one Info line; an active pass
   keeps its detail. A one-shot `sync` with nothing to do prints nothing
   at Info and exits 0.
3. **`Report` gains `version`, `last_success_at`, `consecutive_failures`.**
   All derivable by both the daemon and the CLI. `next_run_at` is
   deliberately not added: it lives in the daemon's memory, so the CLI
   could not report it and the two surfaces would disagree.

## Section 1: max-age

`cmd/difmsync/main.go` gains:

```go
// effectiveMaxAge returns the freshness window the health rule uses.
// Unset, it follows the interval: a probe that is red between every
// pair of passes at a long interval is wrong, not strict. Set, it is
// the operator's number.
func effectiveMaxAge(c *cli.Command) time.Duration {
	if c.IsSet("max-age") {
		return c.Duration("max-age")
	}
	return 3 * c.Duration("interval")
}
```

Both call sites — the daemon's `status.Handler(...)` and the `status`
command's `status.Build(...)` — use it. `statusCommand()` gains
`intervalFlag()` (extracted from `syncCommand`'s inline definition so
the two cannot drift; the config-drift test already asserts a variable
read by two flags has one default). The README row for
`DIFMSYNC_STATUS_MAX_AGE` becomes `` `45m` (unset: 3 × `DIFMSYNC_INTERVAL`) ``
— the test reads the first backticked value, which is unchanged.

`docs/deploy.md`'s "Is it still working?" section drops the instruction
to set `max-age` by hand when changing the interval, and says the rule.

The 20-row scan window (CLAUDE.md, health rule) still applies. At a
derived 3 × interval it is never the binding constraint: 20 rows at
interval *i* span ~20*i*, which exceeds 3*i* for every *i*.

## Section 2: pass logging

`internal/syncer/engine.go`:
- `fetched likes` — `Info` when `len(likes) > 0`, else `Debug`.
- `sync complete` — `Info` when `stats.Fetched > 0`, else `Debug`.
- `dry run complete` — unchanged (a dry run is an operator asking).

`internal/syncer/loop.go`, in `Loop` after each pass, before `wait =
after(delay)`:

```go
e.Log.Info("pass finished",
	"fetched", stats.Fetched, "added", stats.Added,
	"queued", stats.Queued, "skipped", stats.Skipped,
	"clean", err == nil,
	"next_run", e.now().Add(delay).Format(time.RFC3339))
```

`Engine` gains an unexported `now func() time.Time` (nil → `time.Now`)
next to `after`, injected by the Loop tests through `export_test.go`
so the timestamp is assertable. The existing `sync pass failed` Error
line on a failed pass stays: it carries the error text, which the
summary line deliberately does not.

## Section 3: report fields

`internal/status`:

- `health()` returns the qualifying run alongside the verdict, so
  `Build` can set `LastSuccessAt` from the same row the rule used —
  never a second query that could disagree.
- `consecutiveFailures(runs)` counts finished, non-dry, errored rows
  newer than the newest clean non-dry finished row within the scan
  window (in-flight rows are skipped, not counted). Zero when the
  newest qualifying row is the newest finished row.
- `Report` gains, in this order after `Healthy`/`Reason`:
  `Version string json:"version"`,
  `LastSuccessAt string json:"last_success_at,omitempty"`,
  `ConsecutiveFailures int json:"consecutive_failures"`.
- `Build` and `Handler` take `version string`; `main` passes
  `buildVersion()`. The CLI `status` prints `version:` and
  `last success:` lines and a `failures since:` count when non-zero.

`TestReportCarriesNoSecrets` is extended with the new fields in its
fixture (they are derived values, but the test's job is to fail when a
field is added without being looked at).

## Tests

- `TestEffectiveMaxAge`: unset → 3 × interval; set via flag → as set;
  set via env → as set; at the default interval the two agree (45m).
- `TestStatusCommandReadsTheInterval`: the config-drift test's
  duplicate-default assertion covers the new flag; add a case in
  `main_test.go` that `status --check` with `DIFMSYNC_INTERVAL=2h` and
  no max-age accepts a 5h-old clean run and rejects a 7h-old one.
- Loop tests (`engine_test.go`): after a pass, the log buffer contains
  `pass finished` with `next_run` equal to the fake clock's now + delay;
  an idle pass produces exactly one Info line; an active pass still has
  `fetched likes` and `sync complete`.
- Status: `last_success_at` equals the qualifying row; three failed
  rows on top of a clean one → `consecutive_failures=3`; an in-flight
  row is not counted; dry runs are not counted; a clean newest row → 0;
  no clean row in the window → count of failed rows in the window.
- `TestReportCarriesNoSecrets` fixture updated.

## Documentation

README table cell; `docs/deploy.md` "Is it still working?" and the
`/status.json` field list; CLAUDE.md health-rule paragraph gains the
derivation sentence; CHANGELOG `[Unreleased]`.

## Out of scope

`next_run_at` on the endpoint; Prometheus; anything in slices 3–4.
