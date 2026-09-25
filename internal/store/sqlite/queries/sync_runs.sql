-- name: StartSyncRun :one
INSERT INTO sync_runs (account_id, started_at, dry_run)
VALUES (?, ?, ?)
RETURNING id, account_id, started_at, finished_at, dry_run,
          fetched, added, queued, skipped, error, error_kind;

-- name: FinishSyncRun :exec
UPDATE sync_runs
SET finished_at = ?, fetched = ?, added = ?, queued = ?, skipped = ?, error = ?, error_kind = ?
WHERE id = ?;

-- name: ListSyncRuns :many
SELECT id, account_id, started_at, finished_at, dry_run,
       fetched, added, queued, skipped, error, error_kind
FROM sync_runs
WHERE account_id = ?
ORDER BY started_at DESC, id DESC
LIMIT ?;

-- name: PruneSyncRuns :execrows
-- Rows older than the cutoff go, except the newest N, which the health
-- rule reads, and any row still in flight. The inner table is aliased
-- because sqlc otherwise reports the self-reference as ambiguous.
DELETE FROM sync_runs
WHERE sync_runs.account_id = ?
  AND sync_runs.finished_at IS NOT NULL
  AND sync_runs.started_at < ?
  AND sync_runs.id NOT IN (
    SELECT recent.id FROM sync_runs AS recent
    WHERE recent.account_id = ?
    ORDER BY recent.started_at DESC, recent.id DESC
    LIMIT ?
  );
