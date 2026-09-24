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
