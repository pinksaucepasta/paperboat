-- +goose Up
-- A device login can also enroll the device: the CLI says whether this device is already a
-- machine (enrolled_machine_id) and which workspace it asked for; the account owner picks the
-- destination when approving (workspace_team_id, NULL = Personal). The enrollment the approval
-- mints carries that team so the machine joins it when pairing completes.
ALTER TABLE paperboat.device_grants
    ADD COLUMN enroll boolean NOT NULL DEFAULT false,
    ADD COLUMN enrolled_machine_id text,
    ADD COLUMN requested_workspace text,
    ADD COLUMN workspace_team_id text;
ALTER TABLE paperboat.user_machine_pairings
    ADD COLUMN workspace_team_id text;

-- +goose Down
ALTER TABLE paperboat.user_machine_pairings DROP COLUMN workspace_team_id;
ALTER TABLE paperboat.device_grants
    DROP COLUMN workspace_team_id,
    DROP COLUMN requested_workspace,
    DROP COLUMN enrolled_machine_id,
    DROP COLUMN enroll;
