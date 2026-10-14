-- +goose Up
ALTER TABLE paperboat.user_machine_pairings
    DROP CONSTRAINT user_machine_pairings_bootstrap_grant,
    ADD CONSTRAINT user_machine_pairings_bootstrap_grant CHECK (
        (bootstrap_owner_account IS NULL AND operation_id IS NULL AND idempotency_key IS NULL
         AND bootstrap_token_hash IS NULL AND bootstrap_token_ciphertext IS NULL) OR
        (bootstrap_owner_account IS NOT NULL AND operation_id IS NOT NULL AND idempotency_key IS NOT NULL
         AND ((bootstrap_token_hash IS NOT NULL AND bootstrap_token_ciphertext IS NOT NULL) OR
              (installation_state = 'deleted' AND bootstrap_token_hash IS NULL AND bootstrap_token_ciphertext IS NULL))));

-- +goose Down
-- Deleted credentials cannot be restored; roll back the release instead.
-- +goose StatementBegin
DO $$ BEGIN
    RAISE EXCEPTION 'terminal pairing credential cleanup is irreversible; roll back the release instead';
END $$;
-- +goose StatementEnd
