-- Files sent by the nodes (FIL-001, FIL-004, FIL-005): the thumbnail the
-- hub makes of an image file, and the index of the retention (oldest files
-- of a kind first).

-- +goose Up
ALTER TABLE files ADD COLUMN thumbnail BLOB CHECK (thumbnail IS NULL OR length(thumbnail) BETWEEN 1 AND 262144);

CREATE INDEX ix_files_kind_created ON files (kind, created_at);

-- +goose Down
DROP INDEX ix_files_kind_created;
ALTER TABLE files DROP COLUMN thumbnail;
