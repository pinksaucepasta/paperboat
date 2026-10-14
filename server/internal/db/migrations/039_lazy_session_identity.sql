-- +goose Up
-- Lazy activations must be drained before this identity cutover. No identity
-- can be reconstructed from boot metadata or migrated into a live signed lease.
-- +goose StatementBegin
DO $$
BEGIN
 IF EXISTS (SELECT 1 FROM paperboat.lazy_activations) OR EXISTS (SELECT 1 FROM paperboat.preview_leases WHERE owner_session_kind='lazy_runtime' AND terminal_state='active') THEN
  RAISE EXCEPTION 'drain lazy previews and clear inactive lazy activations before applying lazy session identity';
 END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE paperboat.lazy_activations ADD COLUMN owner_session_id text NOT NULL
 CHECK (owner_session_id ~ '^session_[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$');
