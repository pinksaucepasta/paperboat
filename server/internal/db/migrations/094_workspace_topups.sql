-- +goose Up
ALTER TABLE paperboat.billing_products DROP CONSTRAINT billing_products_verified_price_valid;
ALTER TABLE paperboat.billing_products ADD CONSTRAINT billing_products_verified_price_valid CHECK (
 (verified_unit_amount IS NULL AND verified_currency IS NULL AND verified_interval IS NULL) OR
 (verified_unit_amount>=0 AND verified_currency ~ '^[a-z]{3}$' AND
 ((catalog_type='top_up' AND verified_interval='one_time') OR
 (catalog_type<>'top_up' AND verified_interval IN ('month','year')))));
-- Extend the current operation contract without discarding other feature kinds.
-- +goose StatementBegin
DO $$ DECLARE definition text; BEGIN
 SELECT pg_get_constraintdef(oid) INTO definition FROM pg_constraint
 WHERE conrelid='paperboat.billing_workspace_operations'::regclass AND conname='billing_workspace_operations_kind_check';
 EXECUTE 'ALTER TABLE paperboat.billing_workspace_operations DROP CONSTRAINT billing_workspace_operations_kind_check';
 EXECUTE 'ALTER TABLE paperboat.billing_workspace_operations ADD CONSTRAINT billing_workspace_operations_kind_check CHECK (kind=''topup'' OR ' || substring(definition from 7) || ')';
END $$;
-- +goose StatementEnd
CREATE TABLE paperboat.billing_workspace_topups (
 provider_session_id text PRIMARY KEY,
 operation_key text NOT NULL UNIQUE REFERENCES paperboat.billing_workspace_operations(idempotency_key),
 workspace_id text NOT NULL REFERENCES paperboat.billing_workspaces(id),
 purchased_bytes bigint NOT NULL CHECK (purchased_bytes>0),
 remaining_bytes bigint NOT NULL CHECK (remaining_bytes>=0 AND remaining_bytes<=purchased_bytes),
 purchased_at timestamptz NOT NULL
);
CREATE INDEX billing_workspace_topups_remaining ON paperboat.billing_workspace_topups(workspace_id,purchased_at,provider_session_id) WHERE remaining_bytes>0;
CREATE TABLE paperboat.billing_workspace_topup_events (
 event_id text PRIMARY KEY,
 provider_session_id text NOT NULL,
 event_type text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);
