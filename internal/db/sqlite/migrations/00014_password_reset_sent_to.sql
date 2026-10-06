-- The address a password reset link was e-mailed to: completing the reset
-- confirms that address only, never one set afterwards or a link copied by
-- an admin (ADR 0011).

-- +goose Up
ALTER TABLE password_reset_tokens ADD COLUMN sent_to TEXT CHECK (sent_to IS NULL OR length(sent_to) <= 254);

-- +goose Down
ALTER TABLE password_reset_tokens DROP COLUMN sent_to;
