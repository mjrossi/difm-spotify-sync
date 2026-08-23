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

-- Counts rather than returning the token. The only caller is the poll on
-- the daemon consent wait, which needs to know whether consent has landed,
-- not what it was: reading a refresh token into memory every ten seconds
-- is a wider read than the question being asked.
--
-- Keep quote characters out of comments in this file. sqlc v1.28 lexes
-- them as string literals even inside a comment, which shifts every
-- offset after it and silently truncates the generated SQL of this query
-- and the next one. The symptom is a runtime SQL logic error: incomplete
-- input, raised from generated code nobody reads.
-- name: CountSpotifyRefreshToken :one
SELECT COUNT(*) FROM accounts WHERE spotify_refresh_token != '' AND id = ?;

-- name: ClearWatermark :exec
UPDATE accounts SET watermark_liked_at = '' WHERE id = ?;

