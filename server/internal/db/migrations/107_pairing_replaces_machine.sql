-- +goose Up
-- A fresh enrollment may name the record of the computer it replaces (start fresh deletes that record
-- first and the new install arrives later). The hint is a request, never authority: approval only
-- honours it for a retired record of the same account.
ALTER TABLE paperboat.user_machine_pairings
    ADD COLUMN replaces_machine_id text CHECK (replaces_machine_id IS NULL OR length(replaces_machine_id) BETWEEN 1 AND 128);

-- +goose Down
ALTER TABLE paperboat.user_machine_pairings DROP COLUMN replaces_machine_id;
