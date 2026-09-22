# Data Hygiene Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prune `sync_runs` to 90 days (never below the newest 20 rows) after each clean pass; refuse to open a database that fails `PRAGMA quick_check`, with a restore instruction; fuzz `Normalize`, `Parse` and `Score`.

**Architecture:** One new sqlc query and `Store.PruneRuns`; the engine calls it at the end of a clean non-dry pass with two exported constants (`RunsRetention`, `KeepRuns`), the latter asserted equal to `status.HealthScanLimit`. `sqlite.Open` gains a `quick_check` after its ping. `pkg/match/fuzz_test.go` holds three fuzz targets seeded from the existing tables; `just fuzz` runs them for a bounded time. Spec: `docs/superpowers/specs/2026-09-22-data-hygiene-design.md`.

**Tech Stack:** Go 1.26 (`testing.F`), sqlc, modernc.org/sqlite. `mise exec --`, `just`. Read `CLAUDE.md` first — the SQL section (no quote characters in `.sql` comments; a bound parameter as the final token; never hand-edit `gen/`) and Sync semantics (invariant 2: `passClean` is about likes; housekeeping must not touch it).

Branch: `engine-resilience` (PR #10).

---

## File map

| File | Change |
|---|---|
| `internal/store/sqlite/queries/sync_runs.sql` | `PruneSyncRuns :execrows` |
| `internal/store/sqlite/gen/*` | `just gen` |
| `internal/store/sqlite/ops.go` | `PruneRuns` |
| `internal/store/sqlite/store.go` | `quick_check` in `Open` |
| `internal/store/sqlite/store_test.go` | prune tests; corrupt-file test |
| `internal/syncer/engine.go` | prune call at the end of a clean pass |
| `internal/syncer/loop.go` | `RunsRetention`, `KeepRuns` constants |
| `internal/syncer/loop_test.go` | `KeepRuns == status.HealthScanLimit` |
| `internal/syncer/engine_test.go` | prune-on-clean-pass tests |
| `pkg/match/fuzz_test.go` (new) | three fuzz targets |
| `justfile` | `fuzz` recipe |
| `CLAUDE.md`, `docs/deploy.md`, `CHANGELOG.md` | Task 5 |

---

### Task 1: `PruneRuns`

**Files:**
- Modify: `internal/store/sqlite/queries/sync_runs.sql`, `internal/store/sqlite/ops.go`
- Regenerate: `internal/store/sqlite/gen/`
- Test: `internal/store/sqlite/store_test.go`

- [ ] **Step 1: Write the failing store test**

Append to `internal/store/sqlite/store_test.go`:

```go
// TestPruneRunsKeepsTheWindowAndTheInFlightRow: retention is by age, but
// the newest rows survive regardless — the health rule reads them — and
// a row that has not finished is never a candidate, however old its
// start looks to a rewound clock.
func TestPruneRunsKeepsTheWindowAndTheInFlightRow(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	acct, err := s.EnsureAccount(ctx, "default", "111", "p")
	if err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	finished := func(age time.Duration) {
		t.Helper()
		s.SetClock(func() time.Time { return base.Add(-age) })
		id, err := s.StartRun(ctx, acct.ID, false)
		if err != nil {
			t.Fatalf("StartRun: %v", err)
		}
		if err := s.FinishRun(ctx, id, sqlite.RunStats{}); err != nil {
			t.Fatalf("FinishRun: %v", err)
		}
	}
	// 30 finished rows, one per day, the oldest 30 days old.
	for d := 30; d >= 1; d-- {
		finished(time.Duration(d) * 24 * time.Hour)
	}
	// One in-flight row that looks 40 days old.
	s.SetClock(func() time.Time { return base.Add(-40 * 24 * time.Hour) })
	if _, err := s.StartRun(ctx, acct.ID, false); err != nil {
		t.Fatalf("StartRun (in-flight): %v", err)
	}
	s.SetClock(func() time.Time { return base })

	// Retain 10 days, keep at least 5: rows 11..30 days old are
	// candidates (20 rows); the floor of 5 is already satisfied by the
	// newest 10, so all 20 go. The in-flight row stays.
	n, err := s.PruneRuns(ctx, acct.ID, base.Add(-10*24*time.Hour), 5)
	if err != nil {
		t.Fatalf("PruneRuns: %v", err)
	}
	if n != 20 {
		t.Errorf("pruned %d rows, want 20", n)
	}
	runs, err := s.ListRuns(ctx, acct.ID, 100)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 11 {
		t.Fatalf("%d rows remain, want 11 (10 recent + in-flight)", len(runs))
	}
	inFlight := 0
	for _, r := range runs {
		if r.FinishedAt == "" {
			inFlight++
		}
	}
	if inFlight != 1 {
		t.Errorf("in-flight rows remaining = %d, want 1", inFlight)
	}

	// Now retain nothing by age but keep 8: the floor is what saves rows.
	n, err = s.PruneRuns(ctx, acct.ID, base.Add(time.Hour), 8)
	if err != nil {
		t.Fatalf("PruneRuns (floor): %v", err)
	}
	// 11 rows; newest 8 by started_at are kept — the in-flight row is
	// oldest by started_at and is protected by finished_at, not the floor.
	// Candidates: 11 - 8 = 3, minus the in-flight one = 2.
	if n != 2 {
		t.Errorf("pruned %d rows under the floor, want 2", n)
	}
	// Idempotent.
	n, err = s.PruneRuns(ctx, acct.ID, base.Add(time.Hour), 8)
	if err != nil || n != 0 {
		t.Errorf("second prune = (%d, %v), want (0, nil)", n, err)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `mise exec -- go test ./internal/store/sqlite -race -count=1 -run TestPruneRuns`
Expected: FAIL — `s.PruneRuns undefined`.

- [ ] **Step 3: Add the query and regenerate**

Append to `internal/store/sqlite/queries/sync_runs.sql` (no quote characters in the comment):

```sql
-- name: PruneSyncRuns :execrows
-- Rows older than the cutoff go, except the newest N, which the health
-- rule reads, and any row still in flight.
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

Run: `just gen && git status --short internal/store/sqlite/gen`
Expected: `sync_runs.sql.go` modified with `PruneSyncRuns(ctx, PruneSyncRunsParams{AccountID, StartedAt, AccountID_2, Limit}) (int64, error)` (sqlc names the repeated column `AccountID_2`; check the generated struct and use its field names).

- [ ] **Step 4: Add `PruneRuns`**

In `internal/store/sqlite/ops.go`, after `ListRuns`:

```go
// PruneRuns deletes finished runs that started before the cutoff, except
// the newest keep rows. The floor exists because the health rule reads a
// fixed window of rows (status.HealthScanLimit) and must never lose one
// to housekeeping; the in-flight exclusion is what makes it safe to call
// from inside a pass whose own row is still open. Returns the count.
func (s *Store) PruneRuns(ctx context.Context, accountID int64, before time.Time, keep int) (int64, error) {
	n, err := s.q.PruneSyncRuns(ctx, sqlitegen.PruneSyncRunsParams{
		AccountID:   accountID,
		StartedAt:   before.UTC().Format(TimeFormat),
		AccountID_2: accountID,
		Limit:       int64(keep),
	})
	return n, opErr("PruneRuns", err)
}
```

(Adjust field names to what sqlc generated.)

- [ ] **Step 5: Run the store tests**

Run: `mise exec -- go test ./internal/store/sqlite -race -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/store/sqlite migrations-sqlite
git commit -m "Add Store.PruneRuns: by age, never below the newest N, never in flight"
```

---

### Task 2: The engine prunes after a clean pass

**Files:**
- Modify: `internal/syncer/loop.go` (constants), `internal/syncer/engine.go` (after the ledger transaction, before `if !passClean`)
- Test: `internal/syncer/loop_test.go`, `internal/syncer/engine_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/syncer/loop_test.go` (package `syncer`; add `"github.com/mjrossi/difm-spotify-sync/internal/status"` to its imports — `status` does not import `syncer`, so this is not a cycle):

```go
// The prune floor and the health scan window are the same number by
// construction, not by coincidence: pruning below the window would let
// housekeeping delete the row the health rule was about to accept.
func TestKeepRunsIsTheHealthScanWindow(t *testing.T) {
	if KeepRuns != status.HealthScanLimit {
		t.Errorf("KeepRuns = %d, status.HealthScanLimit = %d; they must agree", KeepRuns, status.HealthScanLimit)
	}
}
```

Append to `internal/syncer/engine_test.go`:

```go
// seedOldRuns writes n finished runs with started_at well past the
// retention window, so a clean pass has something to prune.
func seedOldRuns(t *testing.T, h *harness, n int) {
	t.Helper()
	ctx := context.Background()
	old := time.Now().Add(-syncer.RunsRetention - 24*time.Hour)
	h.Store.SetClock(func() time.Time { return old })
	defer h.Store.SetClock(time.Now)
	for i := 0; i < n; i++ {
		id, err := h.Store.StartRun(ctx, h.Engine.Account.ID, false)
		if err != nil {
			t.Fatalf("StartRun: %v", err)
		}
		if err := h.Store.FinishRun(ctx, id, sqlite.RunStats{}); err != nil {
			t.Fatalf("FinishRun: %v", err)
		}
	}
}

func runCount(t *testing.T, h *harness) int {
	t.Helper()
	runs, err := h.Store.ListRuns(context.Background(), h.Engine.Account.ID, 1000)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	return len(runs)
}

// Housekeeping rides on a clean pass and on nothing else: a failed pass
// has better things to do, and a dry run writes nothing by definition.
func TestRunOnce_CleanPassPrunesOldRuns(t *testing.T) {
	h := newHarness(t, nil)
	seedOldRuns(t, h, syncer.KeepRuns+10)

	if _, err := h.Engine.RunOnce(context.Background(), false); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	// KeepRuns newest survive; the pass's own row is among them.
	if got := runCount(t, h); got != syncer.KeepRuns {
		t.Errorf("%d rows after a clean pass, want %d", got, syncer.KeepRuns)
	}
}

func TestRunOnce_FailedAndDryPassesDoNotPrune(t *testing.T) {
	t.Run("failed", func(t *testing.T) {
		h := newHarness(t, []like{aLike(1, "A", "One", 200, feb)})
		h.failSearch["One"] = true
		seedOldRuns(t, h, syncer.KeepRuns+10)
		_, _ = h.Engine.RunOnce(context.Background(), false)
		if got := runCount(t, h); got != syncer.KeepRuns+11 {
			t.Errorf("%d rows after a failed pass, want %d (nothing pruned)", got, syncer.KeepRuns+11)
		}
	})
	t.Run("dry run", func(t *testing.T) {
		h := newHarness(t, nil)
		seedOldRuns(t, h, syncer.KeepRuns+10)
		if _, err := h.Engine.RunOnce(context.Background(), true); err != nil {
			t.Fatalf("RunOnce(dry): %v", err)
		}
		if got := runCount(t, h); got != syncer.KeepRuns+11 {
			t.Errorf("%d rows after a dry run, want %d (nothing pruned)", got, syncer.KeepRuns+11)
		}
	})
}

// A prune failure is logged and swallowed. It is not a like reaching or
// missing durable state, so invariant 2 is not in play: the pass is
// still clean and the watermark still moves.
func TestRunOnce_PruneFailureDoesNotMarkThePassIncomplete(t *testing.T) {
	h := newHarness(t, []like{aLike(1, "DJ Rax", "Air Race (Spiritchaser Remix)", 480, feb)})
	h.searchResult["Air Race"] = []spotifyTrack{
		{ID: "sp1", Artist: "DJ Rax", Title: "Air Race - Spiritchaser Remix", Seconds: 480},
	}
	// A trigger that refuses every delete on sync_runs.
	h.exec(t, `CREATE TRIGGER no_prune BEFORE DELETE ON sync_runs BEGIN SELECT RAISE(ABORT, 'no'); END`)
	seedOldRuns(t, h, syncer.KeepRuns+1)

	if _, err := h.Engine.RunOnce(context.Background(), false); err != nil {
		t.Fatalf("RunOnce returned %v, want nil despite the prune failure", err)
	}
	if got := h.reload(t).WatermarkLikedAt; !got.Equal(feb) {
		t.Errorf("watermark = %s, want %s — the pass was clean", got, feb)
	}
	if !strings.Contains(h.Logs.String(), "could not prune") {
		t.Errorf("prune failure not logged:\n%s", h.Logs.String())
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `mise exec -- go test ./internal/syncer -race -count=1 -run 'TestKeepRuns|TestRunOnce_CleanPassPrunes|TestRunOnce_FailedAndDry|TestRunOnce_PruneFailure'`
Expected: FAIL — `undefined: syncer.RunsRetention`, `KeepRuns`.

- [ ] **Step 3: Implement**

In `internal/syncer/loop.go`, after `MinInterval`:

```go
// RunsRetention is how long sync_runs history is kept. Not a flag: a
// retention period is not a knob a self-hoster needs, and every flag
// is a README row the config-drift test then polices. Ninety days at
// the default 15m interval is under nine thousand rows.
const RunsRetention = 90 * 24 * time.Hour

// KeepRuns is the floor pruning never goes below, whatever the age. It
// is the health scan window: the rule reads that many rows and must
// never lose one to housekeeping. TestKeepRunsIsTheHealthScanWindow
// pins the two together, since this package cannot import status.
const KeepRuns = 20
```

In `internal/syncer/engine.go`, after the ledger transaction block (the `if len(pendingLedge) > 0 || advanceMark { ... }` block) and before `if !passClean {`:

```go
	// Housekeeping rides on a clean, real pass — after the ledger and
	// the watermark, so it can never sit between them. A failure here
	// is logged and swallowed: it is not a like reaching or missing
	// durable state, so invariant 2 is not in play and passClean stays.
	if passClean && !dryRun {
		before := time.Now().Add(-RunsRetention)
		if n, err := e.Store.PruneRuns(ctx, account.ID, before, KeepRuns); err != nil {
			e.Log.Warn("could not prune old sync runs", "err", err)
		} else if n > 0 {
			e.Log.Debug("pruned old sync runs", "count", n, "older_than", before.Format(time.RFC3339))
		}
	}
```

- [ ] **Step 4: Run the package**

Run: `mise exec -- go test ./internal/syncer -race -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/syncer
git commit -m "Prune sync_runs after each clean pass: 90 days, never below the health window"
```

---

### Task 3: `quick_check` on open

**Files:**
- Modify: `internal/store/sqlite/store.go` (`Open`, after the ping)
- Test: `internal/store/sqlite/store_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/store/sqlite/store_test.go`:

```go
// TestOpenRefusesACorruptDatabase: the documented restore is a docker cp,
// and a truncated or half-written copy used to surface as whatever query
// tripped first — from inside a pass, under a restart policy, with no
// file name and no next step. Now the open itself says which file and
// where the runbook is.
func TestOpenRefusesACorruptDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.db")
	s := openAt(t, path)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Overwrite the middle of the file. Page 1 stays intact so SQLite
	// still recognizes the header and reaches the check; a page inside
	// the btree does not.
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open file: %v", err)
	}
	info, err := f.Stat()
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	garbage := bytes.Repeat([]byte{0xFF}, 512)
	if _, err := f.WriteAt(garbage, info.Size()/2); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close file: %v", err)
	}

	_, err = sqlite.Open(path)
	if err == nil {
		t.Fatal("Open succeeded on a corrupt database")
	}
	for _, want := range []string{"integrity check", path, "Restoring"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Open error = %q, want it to contain %q", err, want)
		}
	}
}

// openAt opens and migrates a store at a known path, then seeds enough
// rows that the file spans several pages.
func openAt(t *testing.T, path string) *sqlite.Store {
	t.Helper()
	ctx := context.Background()
	s, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	acct, err := s.EnsureAccount(ctx, "default", "111", "p")
	if err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}
	for i := 0; i < 200; i++ {
		id, err := s.StartRun(ctx, acct.ID, false)
		if err != nil {
			t.Fatalf("StartRun: %v", err)
		}
		if err := s.FinishRun(ctx, id, sqlite.RunStats{Err: errors.New(strings.Repeat("x", 200))}); err != nil {
			t.Fatalf("FinishRun: %v", err)
		}
	}
	return s
}
```

Add `"bytes"` and `"os"` to the test imports if absent.

- [ ] **Step 2: Run to verify it fails**

Run: `mise exec -- go test ./internal/store/sqlite -race -count=1 -run TestOpenRefusesACorruptDatabase`
Expected: FAIL — either `Open succeeded on a corrupt database`, or an error that lacks "integrity check". If the garbage write lands on a free page and SQLite does not notice, increase the seeded rows or write at several offsets (`info.Size()/3`, `/2`, `2*info.Size()/3`); the test must fail before the fix for the right reason.

- [ ] **Step 3: Implement**

In `internal/store/sqlite/store.go` `Open`, after the ping block and before `return &Store{...}`:

```go
	// quick_check is integrity_check without the index pass: cheap enough
	// to run on every open, including the healthcheck's, and it is what
	// turns a truncated docker cp into a message that names the file and
	// the runbook instead of a driver error from inside the first query.
	var verdict string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&verdict); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite.Open: %s: integrity check could not run: %w", path, err)
	}
	if verdict != "ok" {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite.Open: %s failed integrity check (%s); restore from a backup — docs/deploy.md, Restoring",
			path, verdict)
	}
```

(`ctx` is the 2s ping context declared just above; extend its timeout to 10s so a large database on slow storage is not misreported — change `2*time.Second` to `10*time.Second` on that `WithTimeout` and note it in the comment.)

- [ ] **Step 4: Run the package**

Run: `mise exec -- go test ./internal/store/sqlite -race -count=1`
Expected: PASS — every other test is the negative control (healthy databases still open).

- [ ] **Step 5: Commit**

```bash
git add internal/store/sqlite/store.go internal/store/sqlite/store_test.go
git commit -m "Refuse to open a database that fails quick_check, and say where the runbook is"
```

---

### Task 4: Fuzz the matcher

**Files:**
- Create: `pkg/match/fuzz_test.go`
- Modify: `justfile`

- [ ] **Step 1: Write the fuzz targets**

Create `pkg/match/fuzz_test.go`:

```go
package match_test

import (
	"strings"
	"testing"

	"github.com/mjrossi/difm-spotify-sync/pkg/match"
)

// Seeds drawn from the table tests plus the shapes a decoder-defensive
// client can still hand this package: empty, whitespace, unbalanced
// brackets, mixed scripts, and a title that is nothing but markers.
var fuzzSeeds = [][2]string{
	{"Funk D'Void & Berny", "Junkies (Joe Silva Remix)"},
	{"DJ Rax", "Air Race (Spiritchaser Remix)"},
	{"Elements Of Life", "Live Your Life For Today"},
	{"Various Artists", "!!!"},
	{"", ""},
	{"   ", "\t\n"},
	{"A", "("},
	{"Björk feat. 坂本龍一", "Ærø – Radio Edit [Extended Mix] (feat. Ωmega)"},
	{"x", "(Remix) (Radio Edit) [Extended] feat."},
	{"long", strings.Repeat("a very long title ", 600)},
}

// The matcher normalizes whatever the two APIs send. A panic here takes
// the daemon down on every tick that re-reads the same like, so the
// property is simply: never.
func FuzzNormalize(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s[0])
		f.Add(s[1])
	}
	f.Fuzz(func(t *testing.T, s string) {
		once := match.Normalize(s)
		if twice := match.Normalize(once); twice != once {
			t.Errorf("Normalize is not idempotent: %q -> %q -> %q", s, once, twice)
		}
	})
}

func FuzzParse(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, artist, title string) {
		tr := match.Parse(artist, title)
		if match.Normalize(tr.Title) != tr.Title {
			t.Errorf("Parse returned an unnormalized title %q", tr.Title)
		}
		for _, a := range tr.Artists {
			if match.Normalize(a) != a {
				t.Errorf("Parse returned an unnormalized artist %q", a)
			}
		}
	})
}

func FuzzScore(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s[0], s[1], 300, s[0], s[1], 300)
		f.Add(s[0], s[1], 300, "someone else", "another song", 0)
	}
	f.Fuzz(func(t *testing.T, a1, t1 string, d1 int, a2, t2 string, d2 int) {
		want, got := match.Parse(a1, t1), match.Parse(a2, t2)
		sc := match.Score(want, d1, got, d2)
		if sc.Score < 0 || sc.Score > 1 {
			t.Errorf("Score = %v, want within [0, 1]; why: %s", sc.Score, sc.Why)
		}
		// A track against itself, same duration, is a perfect match —
		// unless its title normalizes to nothing, in which case the
		// package deliberately refuses to call it one.
		if a1 == a2 && t1 == t2 && d1 == d2 && want.Title != "" && sc.Score != 1 {
			t.Errorf("identical tracks scored %v, want 1; why: %s", sc.Score, sc.Why)
		}
	})
}
```

- [ ] **Step 2: Run the seeds**

Run: `mise exec -- go test ./pkg/match -race -count=1 -run 'Fuzz' -v 2>&1 | grep -E '^(--- |ok|FAIL)'`
Expected: all three PASS on their seed corpus. If a seed fails, the failure is a real finding: fix the matcher (or, if the property is wrong — e.g. an identical track that legitimately scores below 1 because the duration weight is redistributed — tighten the property and say why in the comment), never delete the seed.

- [ ] **Step 3: Fuzz for real, briefly**

Run: `for f in FuzzNormalize FuzzParse FuzzScore; do mise exec -- go test ./pkg/match -run '^$' -fuzz "^$f\$" -fuzztime 20s || break; done`
Expected: each completes with no crasher. A crasher is written to `pkg/match/testdata/fuzz/<Fuzz…>/`; commit it as a permanent seed alongside the fix.

- [ ] **Step 4: Add the recipe**

In `justfile`, after `test:`:

```just
# fuzz the matcher for a bounded time; seeds already run under `test`
[group('build')]
fuzz TIME="20s":
    #!/usr/bin/env bash
    set -euo pipefail
    for f in FuzzNormalize FuzzParse FuzzScore; do
        mise exec -- go test ./pkg/match -run '^$' -fuzz "^${f}\$" -fuzztime {{TIME}}
    done
```

Run: `just fuzz 5s` — expected: three runs, each ending `PASS`.

- [ ] **Step 5: Commit**

```bash
git add pkg/match/fuzz_test.go justfile
git commit -m "Fuzz Normalize, Parse and Score; seeds run in the gate, just fuzz runs longer"
```

---

### Task 5: Gate and docs

- [ ] `just check` green; `just fuzz 10s` clean.
- [ ] **CLAUDE.md** Sync semantics: a short paragraph — `sync_runs` is pruned after each clean pass to `RunsRetention` (90d), never below `KeepRuns` (= `HealthScanLimit`, pinned by test); not a flag and why; prune failure is logged, not a pass failure, because invariant 2 is about likes. Testing: fuzz seeds run in `just check`; `just fuzz` runs longer; a crasher is committed as a seed with its fix. Deployment/Operator: `sqlite.Open` runs `quick_check` and refuses a corrupt file with the runbook pointer — the healthcheck opens the database too, so a corrupt file also fails `status --check`.
- [ ] **docs/deploy.md** Backups: a snapshot carries at most 90 days of run history. Restoring: a truncated or corrupt copy is refused at start with `failed integrity check … docs/deploy.md, Restoring` in the log; take a fresh backup rather than retrying.
- [ ] **CHANGELOG.md** `[Unreleased]` → Changed: retention; Added: `quick_check` on open, `just fuzz`.
- [ ] Commit `Document run retention, the integrity check and the fuzz targets`.

---

## Self-review

- Spec §1 → Tasks 1, 2. §2 → Task 3. §3 → Task 4. Docs → Task 5.
- Types: `PruneRuns(ctx, accountID int64, before time.Time, keep int) (int64, error)`; constants `syncer.RunsRetention`, `syncer.KeepRuns`; `status.HealthScanLimit` already exported.
- The prune sits after the ledger transaction and before the `!passClean` return, guarded by `passClean && !dryRun` — so it never runs on a failed or dry pass, and never between Spotify write, ledger and watermark.
