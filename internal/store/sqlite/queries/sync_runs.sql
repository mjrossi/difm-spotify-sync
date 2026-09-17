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
