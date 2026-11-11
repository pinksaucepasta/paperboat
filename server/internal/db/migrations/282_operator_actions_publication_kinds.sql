-- +goose Up
SET LOCAL lock_timeout = '5s';
-- publication_block / publication_unblock operator actions (takedown of one hostname). The new check is
-- added NOT VALID and validated in 283, so this statement holds its lock only briefly.
ALTER TABLE paperboat.operator_actions
    DROP CONSTRAINT operator_actions_kind_check,
    ADD CONSTRAINT operator_actions_kind_check CHECK (kind = ANY (ARRAY['account_ban', 'account_unban', 'team_ban', 'team_unban', 'relay_drain', 'relay_resume', 'tunnel_disable', 'tunnel_enable', 'publication_block', 'publication_unblock'])) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.operator_actions
    DROP CONSTRAINT operator_actions_kind_check,
    ADD CONSTRAINT operator_actions_kind_check CHECK (kind = ANY (ARRAY['account_ban', 'account_unban', 'team_ban', 'team_unban', 'relay_drain', 'relay_resume', 'tunnel_disable', 'tunnel_enable'])) NOT VALID;
