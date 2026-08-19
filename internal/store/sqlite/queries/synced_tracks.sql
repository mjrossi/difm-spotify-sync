-- name: RecordSyncedTrack :exec
INSERT INTO synced_tracks (
    account_id, difm_track_id, difm_vote_id, spotify_track_id,
    playlist_id, artist, title, match_score, liked_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(account_id, difm_track_id, playlist_id) DO NOTHING;

-- name: IsTrackSynced :one
SELECT EXISTS (
    SELECT 1 FROM synced_tracks
    WHERE account_id = ? AND difm_track_id = ? AND playlist_id = ?
);

-- name: CountSyncedTracks :one
SELECT COUNT(*) FROM synced_tracks WHERE account_id = ?;

-- No playlist_id predicate: v1 syncs one playlist per account, so the
-- (account_id, difm_track_id) pair is already unique in practice. If the
-- multi-playlist seam in the UNIQUE constraint is ever used, this needs
-- one or it will forget the track from every playlist at once.
-- name: ForgetSyncedTrack :execrows
DELETE FROM synced_tracks
WHERE account_id = ? AND difm_track_id = ?;

-- name: ForgetAllSyncedTracks :exec
DELETE FROM synced_tracks WHERE account_id = ?;

-- Read before ForgetSyncedTrack deletes the row. `resync --forget` has to
-- clear both suppressors — the ledger row and the watermark — and the
-- watermark can only be rewound to the right point if the like's
-- timestamp is known first.
-- name: GetSyncedTrackLikedAt :one
-- :one already takes a single row; an explicit LIMIT here is rewritten
-- by sqlc into invalid SQL.
SELECT liked_at FROM synced_tracks
WHERE account_id = ? AND difm_track_id = ?
ORDER BY liked_at ASC;
