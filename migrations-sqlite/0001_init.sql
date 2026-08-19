-- +goose Up
-- +goose StatementBegin

-- accounts is the multi-tenancy seam. v1 runs with exactly one row
-- ('default'), but every other table carries account_id so adding real
-- multi-user support later is a feature, not a migration of the world.
CREATE TABLE accounts (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    label                 TEXT NOT NULL UNIQUE,
    difm_member_id        TEXT NOT NULL DEFAULT '',
    spotify_playlist_id   TEXT NOT NULL DEFAULT '',
    spotify_refresh_token TEXT NOT NULL DEFAULT '',
    -- Incremental-sync watermark: the created_at of the newest like
    -- successfully processed. Advanced only after a fully clean pass.
    watermark_liked_at    TEXT NOT NULL DEFAULT '',
    created_at            TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

-- synced_tracks is the idempotency ledger. The UNIQUE constraint — not
-- an in-memory set — is what guarantees a second pass adds nothing.
CREATE TABLE synced_tracks (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id       INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    difm_track_id    INTEGER NOT NULL,
    difm_vote_id     INTEGER NOT NULL DEFAULT 0,
    spotify_track_id TEXT NOT NULL,
    playlist_id      TEXT NOT NULL,
    artist           TEXT NOT NULL DEFAULT '',
    title            TEXT NOT NULL DEFAULT '',
    match_score      REAL NOT NULL DEFAULT 0,
    liked_at         TEXT NOT NULL DEFAULT '',
    added_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    UNIQUE (account_id, difm_track_id, playlist_id)
);

-- Serves the ledger listing ordered by liked_at (`just ledger`), which
-- is how an operator finds the DI.fm track id that `resync --forget`
-- needs, plus GetSyncedTrackLikedAt, which that same command uses to
-- rewind the watermark. Note the `just ledger` recipe reaches the index
-- through sqlite3 directly rather than through a Go query.
CREATE INDEX synced_tracks_account_liked
    ON synced_tracks(account_id, liked_at DESC);

-- review_queue holds every like that did NOT auto-add, so nothing is
-- ever silently dropped. reason distinguishes the two cases:
--   low_confidence — plausible candidates exist, below the auto threshold
--   no_match       — Spotify returned nothing usable
--   skipped        — filtered out as a DJ mix / mix-show episode
-- They share one table because they share one shape and one question
-- ("what needs my attention?"); candidates_json is simply empty for the
-- latter two.
CREATE TABLE review_queue (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id      INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    difm_track_id   INTEGER NOT NULL,
    difm_vote_id    INTEGER NOT NULL DEFAULT 0,
    artist          TEXT NOT NULL DEFAULT '',
    title           TEXT NOT NULL DEFAULT '',
    duration_sec    INTEGER NOT NULL DEFAULT 0,
    details_url     TEXT NOT NULL DEFAULT '',
    candidates_json TEXT NOT NULL DEFAULT '[]',
    best_score      REAL NOT NULL DEFAULT 0,
    -- Inline CHECKs cannot be altered in SQLite: adding a fourth reason
    -- or status means a migration that rebuilds the table (create new,
    -- copy, drop, rename, recreate indexes). Worth knowing before you
    -- need it, not after.
    reason          TEXT NOT NULL DEFAULT 'low_confidence'
                    CHECK (reason IN ('low_confidence','no_match','skipped')),
    status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending','approved','rejected')),
    liked_at        TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    resolved_at     TEXT,
    UNIQUE (account_id, difm_track_id)
);

CREATE INDEX review_queue_account_status
    ON review_queue(account_id, status, created_at DESC);

-- sync_runs makes silent failure visible. Without it, "is this actually
-- working?" is unanswerable on a headless box.
CREATE TABLE sync_runs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id  INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    started_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    finished_at TEXT,
    dry_run     INTEGER NOT NULL DEFAULT 0,
    fetched     INTEGER NOT NULL DEFAULT 0,
    added       INTEGER NOT NULL DEFAULT 0,
    queued      INTEGER NOT NULL DEFAULT 0,
    skipped     INTEGER NOT NULL DEFAULT 0,
    error       TEXT NOT NULL DEFAULT ''
);

CREATE INDEX sync_runs_account_started
    ON sync_runs(account_id, started_at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS sync_runs_account_started;
DROP TABLE IF EXISTS sync_runs;
DROP INDEX IF EXISTS review_queue_account_status;
DROP TABLE IF EXISTS review_queue;
DROP INDEX IF EXISTS synced_tracks_account_liked;
DROP TABLE IF EXISTS synced_tracks;
DROP TABLE IF EXISTS accounts;
-- +goose StatementEnd
