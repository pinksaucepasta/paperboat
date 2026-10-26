-- +goose Up
-- A browser terminal counts toward the plan's open browser terminals only while it is attached (user rule
-- 2026-10-26). attached_until is renewed while the edge's control connection for the attachment lives
-- and set to now when it closes; a server that dies leaves it to lapse within one admission lifetime.
-- expires_at stays the authority's own bound for usage receipts.
ALTER TABLE paperboat.native_usage_authorities ADD COLUMN attached_until timestamptz;

-- +goose Down
ALTER TABLE paperboat.native_usage_authorities DROP COLUMN attached_until;
