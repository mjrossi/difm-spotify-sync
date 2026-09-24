# Built-in Backups and a Narrowed Container — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The daemon takes its own verified daily snapshot and keeps the newest N; the container runs with all capabilities dropped except the five the entrypoint needs, proven by CI.

**Architecture:** The safe-snapshot routine moves from `cmd/difmsync/backup.go` into `Store.SnapshotTo` so the engine and the CLI share one path. `Engine.Backups` runs it from the same guarded block as the run prune. `compose.yaml` and the README `docker run` drop capabilities; `container-tests.yml` proves the set with a negative control. Spec: `docs/superpowers/specs/2026-09-24-container-and-backups-design.md`.

**Tech Stack:** Go 1.26, `VACUUM INTO`, Docker/Compose, GitHub Actions. Read `CLAUDE.md` first — Deployment (root window, PUID/PGID, the entrypoint's four named paths), Testing (container assertions need negative controls), and Sync semantics (a snapshot is not a like; `passClean` is untouched).

Branch: `engine-resilience` (PR #10).

---

## File map

| File | Change |
|---|---|
| `internal/store/sqlite/ops.go` | `SnapshotTo` (moved from cmd) |
| `internal/store/sqlite/store_test.go` | snapshot tests |
| `cmd/difmsync/backup.go` | calls `SnapshotTo` |
| `internal/syncer/backup.go` (new) | `Backups`, daily+prune logic |
| `internal/syncer/engine.go` | call it in the guarded block |
| `internal/syncer/backup_test.go` (new) | engine-level tests |
| `cmd/difmsync/main.go` | `--backup-dir`, `--backup-keep`, wire `Backups` |
| `internal/status/status.go` | `last_backup_at` |
| `Dockerfile` | `ENV DIFMSYNC_BACKUP_DIR=/config/backups` |
| `compose.yaml`, `README.md` | capabilities |
| `.github/workflows/container-tests.yml` | capability job + control |
| `CLAUDE.md`, `docs/deploy.md`, `CHANGELOG.md` | Task 6 |

---

### Task 1: Move the snapshot routine into the store

**Files:** `internal/store/sqlite/ops.go`, `cmd/difmsync/backup.go`, `internal/store/sqlite/store_test.go`

This is a **pure move plus one new test**. No behaviour change: `difmsync backup`'s existing tests must pass untouched.

- [ ] **Step 1: Read `cmd/difmsync/backup.go` end to end.** The steps to move, in order: stat-refuses-existing-dest; `MkdirAll(parent, 0o750)`; `MkdirTemp(parent, ".difmsync-backup-")` with a deferred `RemoveAll`; `BackupTo` into `stage/difmsync.db`; `Chmod 0o600`; `verifyBackup` (reopen, `GetAccount`); `Rename`. Every comment explaining *why* moves with its code — they are the record of what bit the straightforward version.

- [ ] **Step 2: Write the failing store tests**

Append to `internal/store/sqlite/store_test.go`:

```go
// TestSnapshotToVerifiesBeforePublishing: the snapshot is what a restore
// copies over the live database, so an unusable one must never reach the
// destination to be mistaken for a good one later.
func TestSnapshotToVerifiesBeforePublishing(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.EnsureAccount(ctx, "default", "111", "p"); err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}
	dir := t.TempDir()

	dest := filepath.Join(dir, "good.db")
	if err := s.SnapshotTo(ctx, dest, "default"); err != nil {
		t.Fatalf("SnapshotTo: %v", err)
	}
	snap, err := sqlite.Open(dest)
	if err != nil {
		t.Fatalf("the snapshot does not open: %v", err)
	}
	defer func() { _ = snap.Close() }()
	if _, err := snap.GetAccount(ctx, "default"); err != nil {
		t.Errorf("the snapshot has no account row: %v", err)
	}

	// A verify that cannot pass must leave nothing behind at all — not a
	// partial file with a plausible name.
	missing := filepath.Join(dir, "bad.db")
	if err := s.SnapshotTo(ctx, missing, "no-such-account"); err == nil {
		t.Fatal("SnapshotTo with an unknown account returned nil")
	}
	if _, err := os.Stat(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a failed verify left %s behind (stat err = %v)", missing, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".difmsync-backup-") {
			t.Errorf("staging directory %s left behind", e.Name())
		}
	}
}

func TestSnapshotToRefusesAnExistingDestination(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.EnsureAccount(ctx, "default", "111", "p"); err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "taken.db")
	if err := os.WriteFile(dest, []byte("existing"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	err := s.SnapshotTo(ctx, dest, "default")
	if err == nil {
		t.Fatal("SnapshotTo overwrote an existing file")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("err = %v, want it to say the destination already exists", err)
	}
	// And the file it refused to overwrite is untouched.
	b, err := os.ReadFile(dest)
	if err != nil || string(b) != "existing" {
		t.Errorf("the existing file was modified: %q, %v", b, err)
	}
}
```

Add `"io/fs"`, `"os"`, `"path/filepath"`, `"strings"` to the test imports if absent.

- [ ] **Step 3: Run to verify they fail**

Run: `mise exec -- go test ./internal/store/sqlite -race -count=1 -run TestSnapshotTo`
Expected: FAIL — `s.SnapshotTo undefined`.

- [ ] **Step 4: Move the routine**

In `internal/store/sqlite/ops.go`, after `BackupTo`, add `SnapshotTo` carrying the moved body and its comments. Signature:

```go
// SnapshotTo writes a verified snapshot to dest: staged in a private
// directory alongside it, permissions restricted before anything is
// published, reopened and checked for the account row, and only then
// renamed into place.
//
// Lives here rather than in the backup command because the daemon takes
// scheduled snapshots through the same steps, and two snapshot paths is
// how one of them silently loses the staging directory.
func (s *Store) SnapshotTo(ctx context.Context, dest, verifyLabel string) error {
```

The verify becomes an unexported helper in this package (it calls `Open` + `GetAccount`, both local). Keep the message wording — including "check --db-path points at the database you meant" — since `difmsync backup`'s output is the thing an operator reads.

Then replace the body of `backup.go`'s action with the stat/mkdir/snapshot call, keeping the command's own success line. `verifyBackup` in `cmd` goes away.

- [ ] **Step 5: Run both packages**

Run: `mise exec -- go test ./internal/store/sqlite ./cmd/... -race -count=1`
Expected: PASS, including every pre-existing `difmsync backup` test with no edits.

- [ ] **Step 6: Commit**

```bash
git add internal/store/sqlite cmd/difmsync/backup.go
git commit -m "Move the verified-snapshot routine into the store

The daemon is about to take scheduled snapshots and cannot import
cmd. Pure move: same steps, same messages, same tests."
```

---

### Task 2: `Engine.Backups` — one snapshot a day, keep N

**Files:** `internal/syncer/backup.go` (new), `internal/syncer/engine.go`, `internal/syncer/backup_test.go` (new)

- [ ] **Step 1: Write the failing tests**

Create `internal/syncer/backup_test.go` (package `syncer_test`):

```go
package syncer_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mjrossi/difm-spotify-sync/internal/syncer"
)

func snapshots(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("ReadDir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "difmsync-") && strings.HasSuffix(e.Name(), ".db") {
			out = append(out, e.Name())
		}
	}
	return out
}

// One a day, not one a pass: at a 15m interval the alternative is 96
// VACUUMs of a database that rarely changed.
func TestRunOnce_TakesOneSnapshotPerDay(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "backups")
	h := newHarness(t, nil)
	h.Engine.Backups = &syncer.Backups{Dir: dir, Keep: 14}

	for range 3 {
		if _, err := h.Engine.RunOnce(ctx, false); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
	}
	got := snapshots(t, dir)
	if len(got) != 1 {
		t.Fatalf("three passes in one day produced %v, want exactly one snapshot", got)
	}
	want := "difmsync-" + time.Now().UTC().Format("2006-01-02") + ".db"
	if got[0] != want {
		t.Errorf("snapshot = %q, want %q", got[0], want)
	}
}

func TestRunOnce_KeepsTheNewestSnapshots(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// Five older days, oldest first. ISO dates sort lexically, which is
	// what the prune relies on instead of stat-ing every file.
	for _, day := range []string{"2026-01-01", "2026-01-02", "2026-01-03", "2026-01-04", "2026-01-05"} {
		if err := os.WriteFile(filepath.Join(dir, "difmsync-"+day+".db"), []byte("old"), 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	h := newHarness(t, nil)
	h.Engine.Backups = &syncer.Backups{Dir: dir, Keep: 3}

	if _, err := h.Engine.RunOnce(ctx, false); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	got := snapshots(t, dir)
	if len(got) != 3 {
		t.Fatalf("snapshots = %v, want 3", got)
	}
	// Today's is newest and must survive; 01-01 and 01-02 must not.
	joined := strings.Join(got, " ")
	if strings.Contains(joined, "2026-01-01") || strings.Contains(joined, "2026-01-02") {
		t.Errorf("snapshots = %v, want the two oldest pruned", got)
	}
	if !strings.Contains(joined, time.Now().UTC().Format("2006-01-02")) {
		t.Errorf("snapshots = %v, want today's among them", got)
	}
}

func TestRunOnce_KeepZeroKeepsEverything(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	for _, day := range []string{"2026-01-01", "2026-01-02"} {
		if err := os.WriteFile(filepath.Join(dir, "difmsync-"+day+".db"), []byte("old"), 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	h := newHarness(t, nil)
	h.Engine.Backups = &syncer.Backups{Dir: dir, Keep: 0}

	if _, err := h.Engine.RunOnce(ctx, false); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := snapshots(t, dir); len(got) != 3 {
		t.Errorf("snapshots = %v, want all 3 (Keep=0 disables pruning)", got)
	}
}

// A snapshot is not a like reaching durable state, so a failure to take
// one is a Warn and the pass is still clean — but it must be loud,
// because a volume that filled is exactly the silent failure this
// project keeps legislating against.
func TestRunOnce_BackupFailureWarnsAndLeavesThePassClean(t *testing.T) {
	ctx := context.Background()
	// A *file* where the directory should be: MkdirAll fails.
	blocked := filepath.Join(t.TempDir(), "backups")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	h := newHarness(t, []like{aLike(1, "DJ Rax", "Air Race (Spiritchaser Remix)", 480, feb)})
	h.searchResult["Air Race"] = []spotifyTrack{
		{ID: "sp1", Artist: "DJ Rax", Title: "Air Race - Spiritchaser Remix", Seconds: 480},
	}
	h.Engine.Backups = &syncer.Backups{Dir: blocked, Keep: 14}

	if _, err := h.Engine.RunOnce(ctx, false); err != nil {
		t.Fatalf("RunOnce returned %v, want nil despite the backup failure", err)
	}
	if got := h.reload(t).WatermarkLikedAt; !got.Equal(feb) {
		t.Errorf("watermark = %s, want %s — the pass was clean", got, feb)
	}
	if !strings.Contains(h.Logs.String(), "could not take a backup") {
		t.Errorf("backup failure not logged:\n%s", h.Logs.String())
	}
}

func TestRunOnce_DryRunAndFailedPassesTakeNoSnapshot(t *testing.T) {
	ctx := context.Background()
	t.Run("dry run", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "backups")
		h := newHarness(t, nil)
		h.Engine.Backups = &syncer.Backups{Dir: dir, Keep: 14}
		if _, err := h.Engine.RunOnce(ctx, true); err != nil {
			t.Fatalf("RunOnce(dry): %v", err)
		}
		if got := snapshots(t, dir); len(got) != 0 {
			t.Errorf("a dry run wrote %v", got)
		}
	})
	t.Run("failed pass", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "backups")
		h := newHarness(t, []like{aLike(1, "A", "One", 200, feb)})
		h.failSearch["One"] = true
		h.Engine.Backups = &syncer.Backups{Dir: dir, Keep: 14}
		_, _ = h.Engine.RunOnce(ctx, false)
		if got := snapshots(t, dir); len(got) != 0 {
			t.Errorf("a failed pass wrote %v", got)
		}
	})
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `mise exec -- go test ./internal/syncer -race -count=1 -run 'TestRunOnce_.*[Ss]napshot|TestRunOnce_Keep|TestRunOnce_BackupFailure'`
Expected: FAIL — `h.Engine.Backups undefined`.

- [ ] **Step 3: Implement**

Create `internal/syncer/backup.go`:

```go
package syncer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// snapshotPrefix and snapshotSuffix bracket a scheduled snapshot's name.
// The date between them is ISO, so a lexical sort is a chronological
// one — which is what lets the prune below read a directory rather than
// stat every file in it.
const (
	snapshotPrefix = "difmsync-"
	snapshotSuffix = ".db"
	snapshotDay    = "2006-01-02"
)

// Backups configures the daemon's own scheduled snapshot. Zero value
// (a nil *Backups) takes none.
//
// It exists because the runbook's answer — a host cron running `docker
// compose exec` — runs as root, so every snapshot it writes and the
// directory it creates are root-owned, which is the failure CLAUDE.md's
// Deployment section says lasts longest. Taken by the daemon, they are
// owned by whoever the daemon already runs as.
type Backups struct {
	// Dir holds the snapshots. Empty means take none.
	Dir string
	// Keep is how many to retain, newest first. Zero or less keeps all.
	Keep int
}

// run takes at most one snapshot per UTC day and prunes older ones.
//
// "At most one" is decided from the directory rather than from stored
// state: today's name either exists or it does not, which survives a
// restart and needs no column. The cost is that deleting today's file by
// hand makes the next pass take another — which is the behaviour an
// operator deleting a backup would expect anyway.
func (b *Backups) run(ctx context.Context, store snapshotter, label string) (string, error) {
	day := time.Now().UTC().Format(snapshotDay)
	dest := filepath.Join(b.Dir, snapshotPrefix+day+snapshotSuffix)
	if _, err := os.Stat(dest); err == nil {
		return "", nil // today's is already taken
	}
	if err := os.MkdirAll(b.Dir, 0o700); err != nil {
		return "", fmt.Errorf("creating the backup directory: %w", err)
	}
	if err := store.SnapshotTo(ctx, dest, label); err != nil {
		return "", err
	}
	if err := b.prune(); err != nil {
		// The snapshot itself landed; say so even though the tidy-up
		// failed, because the snapshot is the part that matters.
		return dest, fmt.Errorf("pruning old snapshots: %w", err)
	}
	return dest, nil
}

// prune deletes all but the newest Keep snapshots.
func (b *Backups) prune() error {
	if b.Keep <= 0 {
		return nil
	}
	entries, err := os.ReadDir(b.Dir)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), snapshotPrefix) && strings.HasSuffix(e.Name(), snapshotSuffix) {
			names = append(names, e.Name())
		}
	}
	if len(names) <= b.Keep {
		return nil
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names))) // ISO dates: lexical is chronological
	for _, name := range names[b.Keep:] {
		if err := os.Remove(filepath.Join(b.Dir, name)); err != nil {
			return err
		}
	}
	return nil
}

// snapshotter is the one store method Backups needs, named here so the
// engine's dependency on the store stays legible.
type snapshotter interface {
	SnapshotTo(ctx context.Context, dest, verifyLabel string) error
}
```

In `internal/syncer/engine.go`, add to `Engine`:

```go
	// Backups, when non-nil, takes one snapshot per UTC day after a
	// clean pass. See backup.go.
	Backups *Backups
```

and inside the existing `if passClean && !dryRun { if ctx.Err() == nil { … } }` block, **before** the prune:

```go
			// Before the run prune, so a snapshot is never taken of a
			// database whose retention has just changed underneath it.
			if e.Backups != nil && e.Backups.Dir != "" {
				if dest, err := e.Backups.run(ctx, e.Store, account.Label); err != nil {
					e.Log.Warn("could not take a backup", "dir", e.Backups.Dir, "err", err)
				} else if dest != "" {
					e.Log.Info("backup written", "path", dest)
				}
			}
```

- [ ] **Step 4: Run the package**

Run: `mise exec -- go test ./internal/syncer -race -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/syncer
git commit -m "Take one verified snapshot a day after a clean pass

The runbook's host cron runs as root, so every snapshot it wrote was
root-owned. Taken here they are owned by whoever the daemon runs as."
```

---

### Task 3: Flags, image default, and wiring

**Files:** `cmd/difmsync/main.go`, `Dockerfile`, `README.md`, `cmd/difmsync/main_test.go`

- [ ] **Step 1: Add the flags** to `syncCommand`:

```go
			&cli.StringFlag{
				Name: "backup-dir",
				Usage: "take one verified snapshot per day into this directory after a clean pass " +
					"(empty disables; the image defaults it to /config/backups)",
				Sources: cli.EnvVars("DIFMSYNC_BACKUP_DIR"),
			},
			&cli.IntFlag{
				Name: "backup-keep", Value: 14,
				Usage:   "how many daily snapshots to keep; 0 keeps every one",
				Sources: cli.EnvVars("DIFMSYNC_BACKUP_KEEP"),
			},
```

- [ ] **Step 2: Wire it** in `newEngine`, on the returned `&syncer.Engine{…}`:

```go
					Backups: &syncer.Backups{
						Dir:  c.String("backup-dir"),
						Keep: c.Int("backup-keep"),
					},
```

- [ ] **Step 3: Image default.** In `Dockerfile`'s `ENV` block add `DIFMSYNC_BACKUP_DIR=/config/backups \` (keep the block's continuation style). `DIFMSYNC_BACKUP_KEEP` gets **no** `ENV` line — its flag default is already 14, and CLAUDE.md's earning rule says only values that differ from the binary's default belong there.

- [ ] **Step 4: README table rows**, in the same order as the flags:

```
| `DIFMSYNC_BACKUP_DIR` | `--backup-dir` | `/config/backups` (CLI: off) |
| `DIFMSYNC_BACKUP_KEEP` | `--backup-keep` | `14` |
```

- [ ] **Step 5: Run the config-drift test**

Run: `mise exec -- go test ./cmd/difmsync -race -count=1 -run TestConfigSurface -v`
Expected: PASS. If it fails, the README cell and the flag/ENV default disagree — fix the doc, not the test.

- [ ] **Step 6: An end-to-end CLI test.** Append to `cmd/difmsync/main_test.go` a test that runs `sync` (one-shot, no `--loop`) against a seeded database with `--backup-dir` pointed at a temp dir and asserts a `difmsync-<today>.db` appears. Use the existing `seed`/`runCLI` helpers; if a one-shot `sync` needs Spotify credentials it cannot have in a test, assert instead that `--backup-dir` reaches `syncer.Backups` by checking the flag is defined with the right default and env source (the drift test covers the rest), and say in a comment why the end-to-end path is not exercised here.

- [ ] **Step 7: Commit**

```bash
git add cmd/difmsync Dockerfile README.md
git commit -m "Wire --backup-dir and --backup-keep; the image backs up by default"
```

---

### Task 4: `last_backup_at` in the report

**Files:** `internal/status/status.go`, `internal/status/http.go`, `cmd/difmsync/main.go`, `internal/status/status_test.go`

- [ ] **Step 1: Write the failing tests** — `Build` with a directory containing `difmsync-2026-01-05.db` and `difmsync-2026-01-06.db` reports `last_backup_at = "2026-01-06"`; an empty or missing directory reports `""`; an unreadable directory reports `""` and does not fail the report; the health verdict is unchanged in every case (a missing backup does not mean syncing stopped).

- [ ] **Step 2: Implement.** `Build` and `Handler` take a `backupDir string` after `version`; `Report` gains `LastBackupAt string json:"last_backup_at,omitempty"` with a comment that it is read from the directory, never a store column — there is no second place for it to disagree with. `main` passes `c.String("backup-dir")` at both call sites. The CLI prints `last backup:` when set. Reuse the name parsing from `internal/syncer/backup.go` by exporting what is needed, **or** duplicate three lines of prefix/suffix matching rather than making `status` import `syncer` — pick one and say why in a comment; do not create an import from `status` to `syncer` (the engine already test-imports `status`, and a cycle would break both).

- [ ] **Step 3: Run** `mise exec -- go test ./internal/status ./cmd/... -race -count=1`.

- [ ] **Step 4: Commit** `Report the newest snapshot's date`.

---

### Task 5: Capabilities, and CI that proves the set

**Files:** `compose.yaml`, `README.md`, `.github/workflows/container-tests.yml`

- [ ] **Step 1: `compose.yaml`**, in the service block:

```yaml
    # The image starts as root to honour PUID/PGID and drops with
    # su-exec, so the root window should be as narrow as the entrypoint
    # actually needs rather than as wide as Docker's defaults. Each
    # capability below names the step that needs it; the set is proven
    # by container-tests.yml, not reasoned about, because a missing one
    # fails at chown in a way `restart: unless-stopped` turns into a
    # loop.
    security_opt:
      - no-new-privileges:true
    cap_drop:
      - ALL
    cap_add:
      - CHOWN        # entrypoint repairs /config and the database by name
      - DAC_OVERRIDE # ...including files a foreign uid owns
      - FOWNER       # chown a file whose owner is not us
      - SETUID       # su-exec drops to PUID
      - SETGID       # ...and to PGID
```

- [ ] **Step 2: README** `docker run` example gains `--security-opt no-new-privileges:true --cap-drop ALL --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add FOWNER --cap-add SETUID --cap-add SETGID`, with one sentence saying why.

- [ ] **Step 3: The CI assertion.** In `.github/workflows/container-tests.yml`, after the existing "PUID/PGID is applied to the database" step, add:

```yaml
      - name: the dropped-capability set is enough to repair a foreign-owned volume
        run: |
          set -euxo pipefail
          vol="$(mktemp -d)"
          # A volume owned by someone else is the case PUID/PGID exists
          # for, and the one that needs CHOWN.
          sudo chown 4242:4242 "$vol"
          docker run --rm \
            --security-opt no-new-privileges:true \
            --cap-drop ALL \
            --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add FOWNER \
            --cap-add SETUID --cap-add SETGID \
            -e PUID=1000 -e PGID=1000 \
            -e DIFMSYNC_DB_PATH=/config/difmsync.db \
            -v "$vol:/config" \
            "$IMAGE" status --check || true
          owner="$(sudo stat -c '%u:%g' "$vol/difmsync.db")"
          test "$owner" = "1000:1000" || { echo "database owned by $owner, want 1000:1000"; exit 1; }

      - name: and CHOWN is load-bearing — the negative control
        run: |
          set -euxo pipefail
          vol="$(mktemp -d)"
          sudo chown 4242:4242 "$vol"
          docker run --rm \
            --cap-drop ALL \
            --cap-add DAC_OVERRIDE --cap-add FOWNER \
            --cap-add SETUID --cap-add SETGID \
            -e PUID=1000 -e PGID=1000 \
            -e DIFMSYNC_DB_PATH=/config/difmsync.db \
            -v "$vol:/config" \
            "$IMAGE" status --check > out.txt 2>&1 || true
          # Without CHOWN the entrypoint cannot repair ownership. Assert
          # the failure rather than the success, or the test above passes
          # whether or not the capability was ever needed.
          grep -qiE 'chown|permission denied|operation not permitted' out.txt \
            || { echo "expected a chown failure without CAP_CHOWN; got:"; cat out.txt; exit 1; }
```

Match the file's existing style for `$IMAGE` and step naming — read the neighbouring steps first. `status --check` is used because it exits non-zero on a fresh volume anyway; what is asserted is the *ownership*, not the exit code.

- [ ] **Step 4: `just lint-workflows`** (actionlint) must pass.

- [ ] **Step 5: Commit** `Drop every capability the entrypoint does not need, and prove the set`.

---

### Task 6: Gate and docs

- [ ] `just check` green; `just fuzz 5s` clean.
- [ ] **docs/deploy.md**: Backups section rewritten — the daemon takes one verified snapshot per UTC day into `DIFMSYNC_BACKUP_DIR` after a clean pass, keeps `DIFMSYNC_BACKUP_KEEP` (14), owned by the service rather than root; **delete the host-cron and `find -mtime` recipes**, which are now the wrong advice; say `difmsync backup --to=` remains for an on-demand copy before an upgrade. Note `status` reports `last backup:`. Volume ownership section: mention the capability set and point at the CI job that proves it.
- [ ] **CLAUDE.md**: Deployment gains the capability set and the rule that it is proven in CI with a negative control, not reasoned about; Sync semantics notes the snapshot shares the prune's guard and runs before it, and that a backup failure is a Warn for the same reason.
- [ ] **CHANGELOG** `[Unreleased]`: Added — scheduled backups, the capability set; Changed — the host-cron recipe is gone.
- [ ] Commit `Document scheduled backups and the capability set`.

---

## Self-review

- Spec §1 → Tasks 1–4. §2 → Task 5. Docs → Task 6.
- Types: `Store.SnapshotTo(ctx, dest, verifyLabel string) error`; `syncer.Backups{Dir string, Keep int}` with unexported `run`/`prune`; `Engine.Backups *Backups`; `status.Build(…, version, backupDir string)`; `status.Handler(store, label, maxAge, version, backupDir, log)`.
- The snapshot call sits inside the existing `passClean && !dryRun && ctx.Err() == nil` block, before the run prune — so it inherits every guard the prune already has and adds none.
