-- +goose Up
-- The sender's progress on a Team Inbox transfer (PLAN 3b File transfer modes): its mode, a transfer
-- state beside the approval status, why it is paused, and the bytes the destination committed. The
-- sender reports it at most every few seconds; total bytes come from the manifest.
ALTER TABLE paperboat.team_inbox_requests
    ADD COLUMN transfer_mode text CHECK (transfer_mode IN ('automatic', 'unlimited')),
    ADD COLUMN transfer_state text CHECK (transfer_state IN ('sending', 'paused', 'done', 'failed')),
    ADD COLUMN pause_reason text CHECK (pause_reason IN ('no_eligible_path', 'destination_offline', 'relay_quota_exhausted')),
    ADD COLUMN committed_bytes bigint NOT NULL DEFAULT 0 CHECK (committed_bytes >= 0),
    ADD COLUMN progress_at timestamptz,
    ADD CONSTRAINT team_inbox_requests_pause_reason_state CHECK ((transfer_state = 'paused') = (pause_reason IS NOT NULL));

-- +goose Down
ALTER TABLE paperboat.team_inbox_requests DROP CONSTRAINT team_inbox_requests_pause_reason_state,
    DROP COLUMN progress_at, DROP COLUMN committed_bytes, DROP COLUMN pause_reason, DROP COLUMN transfer_state, DROP COLUMN transfer_mode;
