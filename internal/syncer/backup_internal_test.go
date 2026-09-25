package syncer

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// landingSnapshotter writes the snapshot file and then makes the backup
// directory read-only, so the snapshot lands and the prune after it
// cannot delete anything — the one outcome RunOnce's real store cannot
// produce on demand.
type landingSnapshotter struct{ dir string }

func (s landingSnapshotter) SnapshotTo(_ context.Context, dest, _ string) error {
	if err := os.WriteFile(dest, []byte("snapshot"), 0o600); err != nil {
		return err
	}
	return os.Chmod(s.dir, 0o500)
}

// A snapshot that landed must be reported as written even when the
// prune after it fails. Reading "could not take a backup" about a backup
// that exists sends the operator after the wrong problem.
func TestTakeReportsALandedSnapshotWhenThePruneFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory mode this test relies on")
	}
	dir := filepath.Join(t.TempDir(), "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) // so TempDir can clean up
	if err := os.WriteFile(filepath.Join(dir, "difmsync-2026-01-01.db"), []byte("old"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var logs bytes.Buffer
	b := &Backups{Dir: dir, Keep: 1}
	b.take(context.Background(), landingSnapshotter{dir: dir}, "default", slog.New(slog.NewTextHandler(&logs, nil)))

	out := logs.String()
	if !strings.Contains(out, "backup written") {
		t.Errorf("landed snapshot not reported as written:\n%s", out)
	}
	if !strings.Contains(out, "could not prune old backups") {
		t.Errorf("prune failure not reported:\n%s", out)
	}
	if strings.Contains(out, "could not take a backup") {
		t.Errorf("a snapshot that landed was reported as not taken:\n%s", out)
	}
}
