-- +goose Up
ALTER TABLE paperboat.user_machines
    DROP CONSTRAINT user_machines_configured_capabilities_check,
    DROP CONSTRAINT user_machines_observed_capabilities_check;

UPDATE paperboat.user_machines
SET configured_capabilities = array_remove(configured_capabilities, 'peer_relay'),
    observed_capabilities = array_remove(observed_capabilities, 'peer_relay'),
    capabilities_desired_version = capabilities_desired_version + 1,
    capabilities_status = CASE WHEN online THEN 'pending' ELSE 'offline' END,
    capabilities_error_code = NULL
WHERE 'peer_relay' = ANY(configured_capabilities)
   OR 'peer_relay' = ANY(observed_capabilities);

ALTER TABLE paperboat.user_machines
    ADD CONSTRAINT user_machines_configured_capabilities_check CHECK (
        configured_capabilities <@ ARRAY['file_receive','preview_launch','terminal_host','codex_host','session_host','ssh_host','keep_awake','environment_injection']::text[]),
    ADD CONSTRAINT user_machines_observed_capabilities_check CHECK (
        observed_capabilities <@ ARRAY['file_receive','preview_launch','terminal_host','codex_host','session_host','ssh_host','keep_awake','environment_injection']::text[]);

-- +goose Down
-- Device peer relay is retired and its desired state cannot be restored safely.
-- +goose StatementBegin
DO $$ BEGIN
    RAISE EXCEPTION 'machine peer-relay removal is irreversible; roll back the release instead';
END $$;
-- +goose StatementEnd
