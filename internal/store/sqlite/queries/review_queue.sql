-- Every column carrying fresh data from the like is refreshed, because a
-- re-queue means DI.fm reported the track again and its metadata may have
-- changed. status and resolved_at are deliberately NOT refreshed: those
-- record a human's decision, and a later re-queue must not silently undo
-- an approval or a rejection.
-- name: EnqueueReview :exec
INSERT INTO review_queue (
    account_id, difm_track_id, difm_vote_id, artist, title,
    duration_sec, details_url, candidates_json, best_score, reason, liked_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(account_id, difm_track_id) DO UPDATE SET
    difm_vote_id    = excluded.difm_vote_id,
    artist          = excluded.artist,
    title           = excluded.title,
    duration_sec    = excluded.duration_sec,
    details_url     = excluded.details_url,
    candidates_json = excluded.candidates_json,
    best_score      = excluded.best_score,
    reason          = excluded.reason,
    liked_at        = excluded.liked_at;

-- Ordered by score so the most likely matches are reviewed first. The
-- index on (account_id, status, created_at DESC) serves the WHERE clause;
-- the ORDER BY sorts in memory, which is fine for a human-sized queue.
-- name: ListReviewQueue :many
SELECT id, account_id, difm_track_id, difm_vote_id, artist, title,
       duration_sec, details_url, candidates_json, best_score, reason,
       status, liked_at, created_at, resolved_at
FROM review_queue
WHERE account_id = ? AND status = ?
ORDER BY best_score DESC, created_at DESC
LIMIT ?;

-- name: ResolveReview :execrows
UPDATE review_queue
SET status = ?, resolved_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
WHERE account_id = ? AND difm_track_id = ?;

-- name: GetReviewItem :one
SELECT id, account_id, difm_track_id, difm_vote_id, artist, title,
       duration_sec, details_url, candidates_json, best_score, reason,
       status, liked_at, created_at, resolved_at
FROM review_queue
WHERE account_id = ? AND difm_track_id = ?;

-- name: CountReviewQueue :one
SELECT COUNT(*) FROM review_queue WHERE account_id = ? AND status = ?;

-- Skipped DJ mixes and mix-show episodes are recorded so nothing is lost,
-- but no human will ever act on them. Counting them as "awaiting review"
-- makes the queue look permanently backed up.
-- name: CountReviewQueueActionable :one
SELECT COUNT(*) FROM review_queue
WHERE account_id = ? AND status = 'pending' AND reason != 'skipped';

