-- +goose Up
SET LOCAL lock_timeout = '5s';
-- entitlement_grant / entitlement_revoke: an operator gives a workspace extra bandwidth or a raised or lowered plan limit,
-- or takes it back. The new check is added NOT VALID and validated in 403, so this statement holds its lock only briefly.
ALTER TABLE paperboat.operator_actions
    DROP CONSTRAINT operator_actions_kind_check,
    ADD CONSTRAINT operator_actions_kind_check CHECK (kind = ANY (ARRAY['account_ban', 'account_unban', 'team_ban', 'team_unban', 'relay_drain', 'relay_resume', 'tunnel_disable', 'tunnel_enable', 'publication_block', 'publication_unblock', 'operator_create', 'operator_disable', 'operator_enable', 'operator_mfa_reset', 'operator_sessions_revoke', 'account_sessions_revoke', 'plan_grant', 'plan_revoke', 'entitlement_grant', 'entitlement_revoke'])) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.operator_actions
    DROP CONSTRAINT operator_actions_kind_check,
    ADD CONSTRAINT operator_actions_kind_check CHECK (kind = ANY (ARRAY['account_ban', 'account_unban', 'team_ban', 'team_unban', 'relay_drain', 'relay_resume', 'tunnel_disable', 'tunnel_enable', 'publication_block', 'publication_unblock', 'operator_create', 'operator_disable', 'operator_enable', 'operator_mfa_reset', 'operator_sessions_revoke', 'account_sessions_revoke', 'plan_grant', 'plan_revoke'])) NOT VALID;
