-- +goose Up
-- +goose StatementBegin

-- error_kind is a category the engine chose from its own sentinels,
-- never text an API returned. The status endpoints answer anyone on the
-- LAN and are forbidden from serving the error column; this is the one
-- thing about a failed pass they are allowed to say. Empty means the
-- pass either succeeded or predates this column.
ALTER TABLE sync_runs ADD COLUMN error_kind TEXT NOT NULL DEFAULT '';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE sync_runs DROP COLUMN error_kind;
-- +goose StatementEnd
