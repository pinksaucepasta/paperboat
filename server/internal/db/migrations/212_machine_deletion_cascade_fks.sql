-- +goose Up
-- DDL only. Final machine deletion cascades through cli sessions, lazy policies and previews; these four
-- FKs still blocked it (NO ACTION). Added NOT VALID here, validated in 213. The advisory lock is the one
-- the preview, tunnel and lazy workers take before touching these tables, so the order is theirs.
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);
-- A consumed grant stays (its consumed_at is the replay guard); only the session link is cleared.
ALTER TABLE paperboat.device_grants
    DROP CONSTRAINT device_grants_cli_client_session_id_fkey,
    ADD CONSTRAINT device_grants_cli_client_session_id_fkey
        FOREIGN KEY (cli_client_session_id) REFERENCES paperboat.cli_client_sessions(id) ON DELETE SET NULL NOT VALID;
-- A redemption only replays the token response of its session within minutes; without the session it is dead data.
ALTER TABLE paperboat.device_token_redemptions
    DROP CONSTRAINT device_token_redemptions_cli_client_session_id_fkey,
    ADD CONSTRAINT device_token_redemptions_cli_client_session_id_fkey
        FOREIGN KEY (cli_client_session_id) REFERENCES paperboat.cli_client_sessions(id) ON DELETE CASCADE NOT VALID;
-- Lazy activations are transient wake records of their policy and preview.
ALTER TABLE paperboat.lazy_activations
    DROP CONSTRAINT lazy_activations_policy_id_fkey,
    ADD CONSTRAINT lazy_activations_policy_id_fkey
        FOREIGN KEY (policy_id) REFERENCES paperboat.lazy_access_policies(id) ON DELETE CASCADE NOT VALID,
    DROP CONSTRAINT lazy_activations_preview_id_fkey,
    ADD CONSTRAINT lazy_activations_preview_id_fkey
        FOREIGN KEY (preview_id) REFERENCES paperboat.preview_leases(id) ON DELETE CASCADE NOT VALID;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'machine deletion FKs are forward-only'; END $$;
-- +goose StatementEnd
