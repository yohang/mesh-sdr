-- Media connections the gateway authz issued (hub_issued = 1), as opposed
-- to rows recorded from node reports: POST /api/v1/auth/token refreshes
-- only the former (ADR 0012).

-- +goose Up
ALTER TABLE connections ADD COLUMN hub_issued INTEGER NOT NULL DEFAULT 0 CHECK (hub_issued IN (0, 1));

-- +goose Down
ALTER TABLE connections DROP COLUMN hub_issued;
