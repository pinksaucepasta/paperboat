-- +goose Up
-- Held directory invitations were stored with expires_at 'infinity', which no reader can scan; they
-- now get the normal invitation lifetime and every directory reconcile renews it (F19-05).
UPDATE paperboat.team_invitations SET expires_at=now()+interval '30 days'
WHERE expires_at='infinity' AND directory_user_id IS NOT NULL;

-- +goose Down
-- Renewed expiries stay; the directory reconcile owns them.
