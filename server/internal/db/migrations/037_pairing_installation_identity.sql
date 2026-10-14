-- +goose Up
-- Unreleased setup state is ephemeral. Drain grants and pairings before this
-- cutover rather than guessing ownership or recreating consumed credentials.
-- +goose StatementBegin
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM paperboat.user_machine_enrollments)
       OR EXISTS (SELECT 1 FROM paperboat.user_machine_pairings) THEN
        RAISE EXCEPTION 'pairing identity cutover requires drained machine enrollment grants and pairings';
    END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE paperboat.user_machine_pairings
    ADD COLUMN bootstrap_owner_account text REFERENCES paperboat.users(id) ON DELETE CASCADE,
    ADD COLUMN operation_id text,
    ADD COLUMN idempotency_key text,
    ADD COLUMN bootstrap_token_hash bytea,
    ADD COLUMN bootstrap_token_ciphertext bytea,
    ADD COLUMN installation_state text NOT NULL,
    ADD COLUMN generation bigint NOT NULL CHECK (generation > 0),
    ADD COLUMN cancelled_at timestamptz,
    ALTER COLUMN verifier_hash DROP NOT NULL,
    ALTER COLUMN user_code DROP NOT NULL,
    ALTER COLUMN requested_alias DROP NOT NULL,
    ALTER COLUMN platform DROP NOT NULL,
    ALTER COLUMN architecture DROP NOT NULL,
    ALTER COLUMN workspace_root DROP NOT NULL,
    ALTER COLUMN public_identity_key DROP NOT NULL,
    DROP CONSTRAINT user_machine_pairings_state_check,
    ADD CONSTRAINT user_machine_pairings_state_check CHECK
        (state IN ('awaiting_bootstrap','pending','approved','denied','expired','consumed')),
    ADD CONSTRAINT user_machine_pairings_bound_device CHECK (
        (state NOT IN ('pending','approved','consumed')
         AND installation_state NOT IN ('approved','material_issued','installing','connecting','ready')) OR
        (verifier_hash IS NOT NULL
         AND user_code IS NOT NULL AND requested_alias IS NOT NULL
         AND platform IS NOT NULL AND architecture IS NOT NULL
         AND workspace_root IS NOT NULL AND public_identity_key IS NOT NULL)),
    ADD CONSTRAINT user_machine_pairings_installation_state CHECK
        (installation_state IN ('awaiting_bootstrap','awaiting_approval','approved','material_issued',
         'installing','connecting','ready','cancelled','expired','denied','failed_retryable',
         'revoked','disconnected','deleted','auth_consumed')),
    ADD CONSTRAINT user_machine_pairings_bootstrap_grant CHECK (
        (bootstrap_owner_account IS NULL AND operation_id IS NULL AND idempotency_key IS NULL
         AND bootstrap_token_hash IS NULL AND bootstrap_token_ciphertext IS NULL) OR
        (bootstrap_owner_account IS NOT NULL AND operation_id IS NOT NULL AND idempotency_key IS NOT NULL
         AND bootstrap_token_hash IS NOT NULL AND bootstrap_token_ciphertext IS NOT NULL));
CREATE UNIQUE INDEX user_machine_pairings_bootstrap_idempotency
    ON paperboat.user_machine_pairings(bootstrap_owner_account,idempotency_key);
CREATE UNIQUE INDEX user_machine_pairings_bootstrap_token
    ON paperboat.user_machine_pairings(bootstrap_token_hash) WHERE bootstrap_token_hash IS NOT NULL;

ALTER TABLE paperboat.auth_enrollment_redemptions
    DROP CONSTRAINT auth_enrollment_redemptions_enrollment_id_fkey;
ALTER TABLE paperboat.auth_enrollment_redemptions
    RENAME COLUMN enrollment_id TO pairing_id;
ALTER TABLE paperboat.auth_enrollment_redemptions
    ADD CONSTRAINT auth_enrollment_redemptions_pairing_id_fkey
    FOREIGN KEY (pairing_id) REFERENCES paperboat.user_machine_pairings(id) ON DELETE CASCADE;
DROP TABLE paperboat.user_machine_enrollments;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
    RAISE EXCEPTION 'pairing installation identity consolidation is irreversible; roll back the release instead';
END $$;
-- +goose StatementEnd
