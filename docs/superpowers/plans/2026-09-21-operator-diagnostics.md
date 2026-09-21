# Operator Diagnostics Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `--max-age` follow `--interval` when unset, collapse an idle loop pass to one Info line that names the next run, and add `version`, `last_success_at` and `consecutive_failures` to the status report.

**Architecture:** A small `effectiveMaxAge(c)` helper in `cmd/difmsync` feeds both the daemon's handler and the `status` command. `RunOnce` demotes its idle lines to Debug and `Loop` emits one `pass finished` line with an injectable clock. `internal/status.health()` returns the qualifying run so `Build` derives `last_success_at` from the same row; `consecutiveFailures` walks the scan window. Spec: `docs/superpowers/specs/2026-09-21-operator-diagnostics-design.md`.

**Tech Stack:** Go 1.26, urfave/cli v3 (`c.IsSet`), `log/slog`. Tooling via `mise exec --` and `just`. Read `CLAUDE.md` first — the Operator surface section is the contract for `internal/status`, and `TestConfigSurfaceIsDocumentedAndConsistent` compares README defaults against the real flags.

Branch: continue on `engine-resilience` (PR #10 is open; these commits extend it).

---

## File map

| File | Change |
|---|---|
| `cmd/difmsync/main.go` | `intervalFlag()` shared by `sync` and `status`; `effectiveMaxAge(c)`; both call sites use it; `version` passed to `Build`/`Handler`; CLI prints the new fields |
| `cmd/difmsync/main_test.go` | `TestEffectiveMaxAge`; a `status --check` case driven by `DIFMSYNC_INTERVAL` |
| `internal/syncer/engine.go` | idle lines to Debug |
| `internal/syncer/loop.go` | `now` field; `pass finished` line |
| `internal/syncer/export_test.go` | `SetNow` |
| `internal/syncer/engine_test.go` | Loop log assertions |
| `internal/status/status.go` | `health()` returns the run; `consecutiveFailures`; `Report` fields; `Build`/`Handler` take `version` |
| `internal/status/http.go` | `Handler(store, label, maxAge, version, log)` |
| `internal/status/status_test.go` | new-field tests; no-secrets fixture |
| `README.md`, `docs/deploy.md`, `CLAUDE.md`, `CHANGELOG.md` | Task 5 |

---

### Task 1: `effectiveMaxAge` and `status --interval`

**Files:**
- Modify: `cmd/difmsync/main.go` (flag definitions ~line 347; `statusCommand` ~line 748; `maxAgeFlag` ~line 318)
- Test: `cmd/difmsync/main_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `cmd/difmsync/main_test.go`:

```go
// TestEffectiveMaxAge: unset, the freshness window follows the interval.
// A fixed 45m at a 2h interval is a probe that is red between every pair
// of passes — wrong, not strict — and restart policies ignore health, so
// nothing visibly breaks. Set, the operator's number wins.
func TestEffectiveMaxAge(t *testing.T) {
	clearEnv(t)
	for _, tc := range []struct {
		name string
		args []string
		env  map[string]string
		want time.Duration
	}{
		{"defaults agree", nil, nil, 45 * time.Minute},
		{"unset follows the interval", []string{"--interval", "2h"}, nil, 6 * time.Hour},
		{"flag wins", []string{"--interval", "2h", "--max-age", "1h"}, nil, time.Hour},
		{"env wins", []string{"--interval", "2h"}, map[string]string{"DIFMSYNC_STATUS_MAX_AGE": "90m"}, 90 * time.Minute},
		{"env interval", nil, map[string]string{"DIFMSYNC_INTERVAL": "30m"}, 90 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			var got time.Duration
			cmd := &cli.Command{
				Flags:  []cli.Flag{intervalFlag(), maxAgeFlag("")},
				Action: func(_ context.Context, c *cli.Command) error { got = effectiveMaxAge(c); return nil },
			}
			if err := cmd.Run(context.Background(), append([]string{"x"}, tc.args...)); err != nil {
				t.Fatalf("run: %v", err)
			}
			if got != tc.want {
				t.Errorf("effectiveMaxAge = %s, want %s", got, tc.want)
			}
		})
	}
}
```

And inside `TestStatusCheckIsTheHealthcheckContract`, after the "stale pass exits non-zero" subtest, add:

```go
	t.Run("max-age follows DIFMSYNC_INTERVAL when unset", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("DIFMSYNC_INTERVAL", "2h")
		dbPath, account := seed(t)
		recordRun(t, dbPath, account.ID, 5*time.Hour)
		if err := runCLI(t, dbPath, "status", "--check"); err != nil {
			t.Errorf("5h-old pass at a 2h interval = %v, want nil (max-age should be 6h)", err)
		}
		dbPath, account = seed(t)
		recordRun(t, dbPath, account.ID, 7*time.Hour)
		if err := runCLI(t, dbPath, "status", "--check"); err == nil {
			t.Error("7h-old pass at a 2h interval = nil, want an error")
		}
	})
```

(`seed` and `recordRun` are the helpers already in that test; `clearEnv` is at `main_test.go:119`. Add `"github.com/urfave/cli/v3"` to the test imports if absent.)

- [ ] **Step 2: Run to verify they fail**

Run: `mise exec -- go test ./cmd/difmsync -race -count=1 -run 'TestEffectiveMaxAge|TestStatusCheckIsTheHealthcheckContract'`
Expected: FAIL — `undefined: intervalFlag`, `undefined: effectiveMaxAge`.

- [ ] **Step 3: Implement**

In `cmd/difmsync/main.go`, next to `maxAgeFlag`:

```go
// intervalFlag is shared by sync and status. status needs it only to
// derive max-age (see effectiveMaxAge); one definition keeps the two
// defaults from drifting, which the config-drift test also asserts.
func intervalFlag() cli.Flag {
	return &cli.DurationFlag{
		Name: "interval", Value: 15 * time.Minute,
		Usage:   "how often the loop runs a pass",
		Sources: cli.EnvVars("DIFMSYNC_INTERVAL"),
	}
}

// effectiveMaxAge returns the freshness window the health rule uses.
// Unset, it follows the interval: a probe that is red between every
// pair of passes at a long interval is wrong, not strict, and restart
// policies ignore health, so the mistake is silent. Set, it is the
// operator's number. The declared default stays 45m, which is 3 × the
// default interval — so the README, the Dockerfile and the config-drift
// test are untouched and an unchanged deployment sees no difference.
func effectiveMaxAge(c *cli.Command) time.Duration {
	if c.IsSet("max-age") {
		return c.Duration("max-age")
	}
	return 3 * c.Duration("interval")
}
```

Replace the inline `&cli.DurationFlag{Name: "interval", ...}` in `syncCommand`'s flags with `intervalFlag(),`. Add `intervalFlag(),` to `statusCommand`'s flags. Replace `c.Duration("max-age")` with `effectiveMaxAge(c)` at both call sites (`status.Handler(...)` in `syncCommand` and `status.Build(...)` in `statusCommand`). Update `maxAgeFlag`'s usage strings if they say "45m".

- [ ] **Step 4: Run to verify they pass, and that the config-drift test still does**

Run: `mise exec -- go test ./cmd/difmsync -race -count=1`
Expected: PASS, including `TestConfigSurfaceIsDocumentedAndConsistent` (the `DIFMSYNC_INTERVAL` variable is now read by two flags with one default; `DIFMSYNC_STATUS_MAX_AGE`'s declared default is unchanged).

- [ ] **Step 5: Commit**

```bash
git add cmd/difmsync/main.go cmd/difmsync/main_test.go
git commit -m "Derive max-age from the interval when it is not set

A fixed 45m at a 2h interval is a probe that is red between every
pair of passes, and restart policies ignore health, so the mistake
was silent. The declared default is unchanged: 45m is 3 × 15m."
```

---

### Task 2: One `pass finished` line per loop pass

**Files:**
- Modify: `internal/syncer/engine.go` (lines ~176 and ~388), `internal/syncer/loop.go` (`Engine` struct is in `engine.go`; `Loop` in `loop.go`), `internal/syncer/export_test.go`
- Test: `internal/syncer/engine_test.go`

- [ ] **Step 1: Write the failing Loop tests**

Append to `internal/syncer/engine_test.go` (the file already has `fakeAfter`, `startLoop`, and the harness `Logs` buffer):

```go
// infoLines returns the Info-level lines in the harness log.
func infoLines(h *harness) []string {
	var out []string
	for _, line := range strings.Split(h.Logs.String(), "\n") {
		if strings.Contains(line, "level=INFO") {
			out = append(out, line)
		}
	}
	return out
}

// An idle pass used to cost two Info lines that said nothing and did not
// say when the next attempt was. Now it is one line that says both.
func TestLoop_IdlePassLogsOneLineWithNextRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newHarness(t, nil)
	clock := newFakeAfter()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	syncer.SetNow(h.Engine, func() time.Time { return now })
	const interval = 2 * time.Hour

	done := startLoop(ctx, h, clock, interval)
	<-clock.asked
	h.Logs.Reset() // drop "starting sync loop"
	clock.fire <- time.Time{}
	<-clock.asked
	cancel()
	<-done

	lines := infoLines(h)
	if len(lines) != 1 || !strings.Contains(lines[0], "pass finished") {
		t.Fatalf("idle pass logged %d Info line(s), want exactly one 'pass finished':\n%s", len(lines), h.Logs.String())
	}
	want := now.Add(interval).Format(time.RFC3339)
	if !strings.Contains(lines[0], "next_run="+want) {
		t.Errorf("line = %q, want next_run=%s", lines[0], want)
	}
	if !strings.Contains(lines[0], "clean=true") || !strings.Contains(lines[0], "fetched=0") {
		t.Errorf("line = %q, want clean=true fetched=0", lines[0])
	}
}

// An active pass keeps its detail: the summary line is in addition to,
// not instead of, the lines that say what was matched.
func TestLoop_ActivePassKeepsItsDetailLines(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newHarness(t, []like{aLike(1, "DJ Rax", "Air Race (Spiritchaser Remix)", 480, feb)})
	h.searchResult["Air Race"] = []spotifyTrack{
		{ID: "sp1", Artist: "DJ Rax", Title: "Air Race - Spiritchaser Remix", Seconds: 480},
	}
	clock := newFakeAfter()

	done := startLoop(ctx, h, clock, 2*time.Hour)
	<-clock.asked
	h.Logs.Reset()
	clock.fire <- time.Time{}
	<-clock.asked
	cancel()
	<-done

	log := h.Logs.String()
	for _, want := range []string{"fetched likes", "sync complete", "pass finished", "added=1"} {
		if !strings.Contains(log, want) {
			t.Errorf("active pass log lacks %q:\n%s", want, log)
		}
	}
}

// A one-shot with nothing to do says nothing at Info: the exit code is
// the answer, and a cron or CI caller has nothing to read.
func TestRunOnce_IdlePassIsQuietAtInfo(t *testing.T) {
	h := newHarness(t, nil)
	if _, err := h.Engine.RunOnce(context.Background(), false); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if lines := infoLines(h); len(lines) != 0 {
		t.Errorf("idle one-shot logged at Info:\n%s", strings.Join(lines, "\n"))
	}
}
```

Add to `internal/syncer/export_test.go`:

```go
// SetNow replaces the clock Loop stamps next_run with.
func SetNow(e *Engine, now func() time.Time) {
	e.now = now
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `mise exec -- go test ./internal/syncer -race -count=1 -run 'TestLoop_IdlePass|TestLoop_ActivePass|TestRunOnce_IdlePass'`
Expected: FAIL — `e.now undefined`.

- [ ] **Step 3: Implement**

In `internal/syncer/engine.go`, add to `Engine` after `after`:

```go
	// now is the clock Loop stamps next_run with; nil means time.Now.
	now func() time.Time
```

Change the `fetched likes` line (~176) to:

```go
	// Info only when there is something to say. At a long interval an
	// idle pass every tick is the whole log, and "count=0" answers
	// nothing; Loop's summary line is the heartbeat.
	fetchedLevel := slog.LevelDebug
	if len(likes) > 0 {
		fetchedLevel = slog.LevelInfo
	}
	e.Log.Log(ctx, fetchedLevel, "fetched likes", "count", len(likes), "since", account.WatermarkLikedAt, "dry_run", dryRun)
```

Change the `sync complete` line (~388) to:

```go
	completeLevel := slog.LevelDebug
	if stats.Fetched > 0 {
		completeLevel = slog.LevelInfo
	}
	e.Log.Log(ctx, completeLevel, "sync complete",
		"added", stats.Added, "queued", stats.Queued, "skipped", stats.Skipped)
```

In `internal/syncer/loop.go`, in `Loop`, bind the clock next to `after`:

```go
	now := e.now
	if now == nil {
		now = time.Now
	}
```

and in the `case <-wait:` branch, change `_, err := e.RunOnce(ctx, dryRun)` to `stats, err := e.RunOnce(ctx, dryRun)`, and after computing `delay` (and its Warn) add, before `wait = after(delay)`:

```go
			// The one line an idle pass leaves at Info. It carries the
			// counts and the next attempt; the error text, when there is
			// one, is on the "sync pass failed" line above and not here.
			e.Log.Info("pass finished",
				"fetched", stats.Fetched, "added", stats.Added,
				"queued", stats.Queued, "skipped", stats.Skipped,
				"clean", err == nil,
				"next_run", now().Add(delay).Format(time.RFC3339))
```

- [ ] **Step 4: Run the package**

Run: `mise exec -- go test ./internal/syncer -race -count=1`
Expected: PASS. Note `TestRunOnce_DiFMUnauthorizedIsTypedAndLogged` asserts an Error line, unaffected.

- [ ] **Step 5: Commit**

```bash
git add internal/syncer
git commit -m "One Info line per loop pass, naming the next run

An idle pass logged two lines that said nothing and not when the next
attempt was. RunOnce keeps its detail at Info only when there is
detail; Loop says fetched/added/queued/skipped, clean, and next_run."
```

---

### Task 3: `version`, `last_success_at`, `consecutive_failures`

**Files:**
- Modify: `internal/status/status.go` (`Report` ~45; `Build` ~116; `assemble` ~217; `health` ~271), `internal/status/http.go` (`Handler` ~32)
- Modify: `cmd/difmsync/main.go` (both call sites; CLI printing ~800)
- Test: `internal/status/status_test.go`

- [ ] **Step 1: Write the failing status tests**

Append to `internal/status/status_test.go`:

```go
// The three numbers a probe wants next to the boolean: when the last
// success was, how many failures have stacked since, and which binary
// is answering.
func TestReportCarriesSuccessTimeAndFailureCount(t *testing.T) {
	ctx := context.Background()
	s, account := newStore(t)
	recordRun(t, s, account.ID, 40*time.Minute, false, nil)         // clean
	recordFailedRun(t, s, account.ID, 30*time.Minute, sqlite.KindError, errPass)
	recordRun(t, s, account.ID, 20*time.Minute, true, nil)          // dry run: not counted
	recordFailedRun(t, s, account.ID, 10*time.Minute, sqlite.KindRateLimited, errPass)
	// An in-flight row: started, never finished. Not counted either.
	if _, err := s.StartRun(ctx, account.ID, false); err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	rep, err := status.Build(ctx, s, testLabel, testMaxAge, 0, "v9.9.9-test")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if rep.Version != "v9.9.9-test" {
		t.Errorf("Version = %q", rep.Version)
	}
	if rep.ConsecutiveFailures != 2 {
		t.Errorf("ConsecutiveFailures = %d, want 2 (dry run and in-flight row excluded)", rep.ConsecutiveFailures)
	}
	if rep.LastSuccessAt == "" {
		t.Fatal("LastSuccessAt empty with a clean run recorded")
	}
	at, err := time.Parse(sqlite.TimeFormat, rep.LastSuccessAt)
	if err != nil {
		t.Fatalf("LastSuccessAt = %q, not %s", rep.LastSuccessAt, sqlite.TimeFormat)
	}
	if age := time.Since(at); age < 39*time.Minute || age > 41*time.Minute {
		t.Errorf("LastSuccessAt is %s old, want ~40m", age)
	}
	if !rep.Healthy {
		t.Error("Healthy = false with a 40m-old clean run and a 45m window")
	}
}

func TestConsecutiveFailuresIsZeroWhenTheNewestRunIsClean(t *testing.T) {
	s, account := newStore(t)
	recordFailedRun(t, s, account.ID, 20*time.Minute, sqlite.KindError, errPass)
	recordRun(t, s, account.ID, 10*time.Minute, false, nil)
	rep, err := status.Build(context.Background(), s, testLabel, testMaxAge, 0, "")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if rep.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures = %d, want 0", rep.ConsecutiveFailures)
	}
}

func TestConsecutiveFailuresWithNoCleanRunCountsTheWindow(t *testing.T) {
	s, account := newStore(t)
	for i := 1; i <= 3; i++ {
		recordFailedRun(t, s, account.ID, time.Duration(i)*time.Minute, sqlite.KindError, errPass)
	}
	rep, err := status.Build(context.Background(), s, testLabel, testMaxAge, 0, "")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if rep.ConsecutiveFailures != 3 || rep.LastSuccessAt != "" {
		t.Errorf("ConsecutiveFailures = %d, LastSuccessAt = %q; want 3 and empty", rep.ConsecutiveFailures, rep.LastSuccessAt)
	}
}
```

Update every existing `status.Build(ctx, s, testLabel, testMaxAge, N)` call in the test file to add a trailing `""` version argument, and every `status.Handler(s, testLabel, testMaxAge, discardLogger())` to `status.Handler(s, testLabel, testMaxAge, "", discardLogger())`. In `TestReportCarriesNoSecrets`, add the three new fields to whatever fixture/expected structure it asserts over (read the test; it exists so a new field cannot be added without being looked at).

- [ ] **Step 2: Run to verify they fail**

Run: `mise exec -- go test ./internal/status -race -count=1`
Expected: FAIL — `too many arguments in call to status.Build`.

- [ ] **Step 3: Implement**

In `internal/status/status.go`:

`Report` gains, after `Reason`:

```go
	// Version is what the answering binary calls itself. A probe that
	// sees a stale healthy report wants to know which build produced it.
	Version string `json:"version"`
	// LastSuccessAt is the finished_at of the run the health rule
	// accepted — the same row, never a second query that could disagree.
	LastSuccessAt string `json:"last_success_at,omitempty"`
	// ConsecutiveFailures counts finished, non-dry, errored runs newer
	// than that row, within the scan window. In-flight rows are skipped.
	ConsecutiveFailures int `json:"consecutive_failures"`
```

Change `health` to return the accepted run:

```go
func health(runs []sqlite.SyncRun, maxAge time.Duration, now time.Time) (healthy bool, reason string, accepted *sqlite.SyncRun) {
```

returning `true, "", &run` on the accepted row (`run := run` inside the loop before taking its address, or index into `runs[i]`), and `false, reason, nil` on every other path. Note the stale case (`age > maxAge`) also returns the row: it is still the last success, just too old — so `last_success_at` is reported alongside the unhealthy reason.

Add:

```go
// consecutiveFailures counts the finished, non-dry, errored runs newer
// than the accepted row (or all of them in the window when there is
// none). In-flight rows are skipped, not counted: a pass that is running
// has not failed yet.
func consecutiveFailures(runs []sqlite.SyncRun, accepted *sqlite.SyncRun) int {
	n := 0
	for i := range runs {
		run := &runs[i]
		if accepted != nil && run.ID == accepted.ID {
			break
		}
		if run.DryRun || run.FinishedAt == "" || run.Error == "" {
			continue
		}
		n++
	}
	return n
}
```

`Build` gains a trailing `version string` parameter, calls `health` for the three results, computes `failures := consecutiveFailures(runs, accepted)` **before** truncating `runs` to `runLimit`, and passes `version`, `accepted`, `failures` into `assemble`, which sets the three fields (`LastSuccessAt: accepted.FinishedAt` when non-nil).

In `internal/status/http.go`, `Handler` gains `version string` before `log` and passes it to `Build`.

In `cmd/difmsync/main.go`: `status.Handler(store, c.String("account"), effectiveMaxAge(c), buildVersion(), log)`; `status.Build(ctx, store, c.String("account"), effectiveMaxAge(c), c.Int("limit"), buildVersion())`. In the table printer, after the `health:` line:

```go
	fmt.Printf("version:   %s\n", rep.Version)
	if rep.LastSuccessAt != "" {
		fmt.Printf("last ok:   %s\n", rep.LastSuccessAt)
	}
	if rep.ConsecutiveFailures > 0 {
		fmt.Printf("failures:  %d since the last clean pass\n", rep.ConsecutiveFailures)
	}
```

- [ ] **Step 4: Run status and cmd tests**

Run: `mise exec -- go test ./internal/status ./cmd/... -race -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/status cmd/difmsync/main.go
git commit -m "Report version, last success time and failures since

The three numbers a probe wants next to the boolean. last_success_at
is the row the health rule accepted, never a second query."
```

---

### Task 4: Gate

- [ ] `just check` green.
- [ ] Manual: `DIFMSYNC_INTERVAL=2h difmsync status --check` against a database whose newest clean run is 5h old exits 0; `difmsync status` prints `version:`, `last ok:`; `/status.json` shows the three fields.

---

### Task 5: Documentation

- [ ] **README.md** table row: `` | `DIFMSYNC_STATUS_MAX_AGE` | `--max-age` | `45m` (unset: 3 × `DIFMSYNC_INTERVAL`) | ``. The endpoints table row for `/status.json` unchanged.
- [ ] **docs/deploy.md** "Is it still working?": replace "(45m by default — three ticks of the 15m interval, so one missed pass is tolerated and two are not)" with "(unset, three times `DIFMSYNC_INTERVAL` — 45m at the default 15m — so one missed pass is tolerated and two are not, whatever the interval; set it to override)". Add a short field list after the `curl … status.json` line: `version`, `healthy`/`reason`, `last_success_at`, `consecutive_failures`, `runs[].error_kind`. Note in the log-reading guidance that an idle pass is one `pass finished` line with `next_run`.
- [ ] **CLAUDE.md** health-rule paragraph: one sentence — `--max-age` unset is 3 × interval, decided in `effectiveMaxAge` for both the daemon and `status --check` so the two cannot disagree; the declared default stays 45m for the config-drift test. Operator surface: `Report` carries `version`, `last_success_at` (the accepted row's time, not a second query), `consecutive_failures`.
- [ ] **CHANGELOG.md** `[Unreleased]` → Changed: the three bullets.
- [ ] `just check`; commit `Document max-age derivation, the pass summary line and the new status fields`.

---

## Self-review

- Spec §1 → Task 1 (+ docs Task 5). §2 → Task 2. §3 → Task 3. Tests listed in the spec: `TestEffectiveMaxAge` ✓, `status --check` interval case ✓, Loop log assertions ✓, status field tests ✓, no-secrets fixture ✓.
- Types: `Build(ctx, store, label, maxAge, runLimit, version)`; `Handler(store, label, maxAge, version, log)`; `health(runs, maxAge, now) (bool, string, *sqlite.SyncRun)`; `consecutiveFailures(runs, accepted)`; `effectiveMaxAge(c)`; `intervalFlag()`; `SetNow`.
