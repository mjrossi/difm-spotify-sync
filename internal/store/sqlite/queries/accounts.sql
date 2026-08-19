-- name: GetAccountByLabel :one
SELECT id, label, difm_member_id, spotify_playlist_id,
       spotify_refresh_token, watermark_liked_at, created_at
FROM accounts
WHERE label = ?;

-- name: UpsertAccount :one
INSERT INTO accounts (label, difm_member_id, spotify_playlist_id)
VALUES (?, ?, ?)
ON CONFLICT(label) DO UPDATE SET
    difm_member_id      = excluded.difm_member_id,
    spotify_playlist_id = excluded.spotify_playlist_id
RETURNING id, label, difm_member_id, spotify_playlist_id,
          spotify_refresh_token, watermark_liked_at, created_at;

-- name: SetSpotifyRefreshToken :exec
UPDATE accounts SET spotify_refresh_token = ? WHERE id = ?;

-- name: SetWatermark :exec
UPDATE accounts SET watermark_liked_at = ? WHERE id = ?;

-- name: ClearWatermark :exec
UPDATE accounts SET watermark_liked_at = '' WHERE id = ?;
