-- +goose Up
SET LOCAL lock_timeout = '5s';
-- The extra bandwidth an operator granted a workspace and that still runs. Every reader of a workspace's allowance (the
-- debit that admits traffic, the exhaustion gate on hot paths, the billing overview) adds this to the period's included bytes.
-- +goose StatementBegin
CREATE FUNCTION paperboat.workspace_granted_bytes(ws text) RETURNS bigint LANGUAGE sql STABLE AS $$
 SELECT coalesce(sum(amount),0)::bigint FROM paperboat.billing_workspace_grants
 WHERE workspace_id=ws AND kind='bandwidth' AND revoked_at IS NULL AND (ends_at IS NULL OR ends_at>now());
$$;
-- +goose StatementEnd

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP FUNCTION paperboat.workspace_granted_bytes(text);
