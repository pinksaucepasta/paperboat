-- +goose Up
-- F17: an open console inspector channel keeps its credential alive by renewing it, instead of being closed and
-- re-admitted when the page swaps credentials. Each term stays short: a credential ends at most 5 minutes after it was
-- issued or last renewed, and a renewal re-resolves the holder's authority.
ALTER TABLE paperboat.inspector_credentials ADD COLUMN renewed_at timestamp with time zone;
ALTER TABLE paperboat.inspector_credentials DROP CONSTRAINT inspector_credentials_check;
ALTER TABLE paperboat.inspector_credentials ADD CONSTRAINT inspector_credentials_check
    CHECK (expires_at > issued_at AND expires_at <= coalesce(renewed_at, issued_at) + interval '5 minutes');
