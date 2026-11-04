-- +goose Up
-- Begin stores resource_kind machine_service_pending for a host under the machine-service domain that has no
-- route yet; the check rejected it and every browser sign-in there failed 503. Added NOT VALID, validated in 219.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.browser_access_transactions DROP CONSTRAINT browser_access_transactions_resource_kind_check;
ALTER TABLE paperboat.browser_access_transactions ADD CONSTRAINT browser_access_transactions_resource_kind_check
  CHECK (resource_kind IN ('preview', 'tunnel', 'lazy_policy', 'machine_service_pending')) NOT VALID;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'browser transaction pending kind is forward-only'; END $$;
-- +goose StatementEnd
