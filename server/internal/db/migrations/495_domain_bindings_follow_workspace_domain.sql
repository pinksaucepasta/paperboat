-- +goose Up
-- One source of truth for serving a tunnel or preview custom hostname: the workspace domain it was bound through.
-- Every reader (ingress, browser access, certificate issuance) already requires the binding's own
-- ownership_state = 'verified'; these triggers keep that state equal to the workspace domain's, and withdraw the
-- bindings when the workspace domain is deleted (removed, or taken over after it lapsed) instead of orphaning them.
SET LOCAL lock_timeout = '5s';

-- +goose StatementBegin
CREATE FUNCTION paperboat.workspace_domain_bindings_follow_state() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    state text := CASE WHEN NEW.ownership_state = 'verified' THEN 'verified' ELSE 'failed' END;
BEGIN
    UPDATE paperboat.tunnel_domains SET ownership_state = state, generation = generation + 1, updated_at = now()
    WHERE workspace_domain_id = NEW.id AND deleted_at IS NULL AND ownership_state IN ('verified', 'failed')
      AND ownership_state <> state;
    UPDATE paperboat.preview_domains SET ownership_state = state, generation = generation + 1, updated_at = now()
    WHERE workspace_domain_id = NEW.id AND deleted_at IS NULL AND ownership_state IN ('verified', 'failed')
      AND ownership_state <> state;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION paperboat.workspace_domain_bindings_withdraw() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE paperboat.tunnel_domains SET ownership_state = 'revoked',
        certificate_state = CASE WHEN certificate_state = 'not_applicable' THEN 'not_applicable' ELSE 'revoked' END,
        conflict_state = 'quarantined', quarantine_until = now(), -- already over: the next owner may bind at once
        deleted_at = now(), generation = generation + 1, updated_at = now()
    WHERE workspace_domain_id = OLD.id AND deleted_at IS NULL;
    UPDATE paperboat.preview_domains SET ownership_state = 'revoked',
        certificate_state = CASE WHEN certificate_state = 'not_applicable' THEN 'not_applicable' ELSE 'revoked' END,
        conflict_state = 'quarantined', quarantine_until = now(), -- already over: the next owner may bind at once
        deleted_at = now(), generation = generation + 1, updated_at = now()
    WHERE workspace_domain_id = OLD.id AND deleted_at IS NULL;
    RETURN OLD;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER bindings_follow_state AFTER UPDATE OF ownership_state ON paperboat.workspace_domains
    FOR EACH ROW WHEN (OLD.ownership_state IS DISTINCT FROM NEW.ownership_state)
    EXECUTE FUNCTION paperboat.workspace_domain_bindings_follow_state();
CREATE TRIGGER bindings_withdraw BEFORE DELETE ON paperboat.workspace_domains
    FOR EACH ROW EXECUTE FUNCTION paperboat.workspace_domain_bindings_withdraw();

-- A live binding always has its workspace domain; the FK's SET NULL only reaches rows withdrawn above.
ALTER TABLE paperboat.tunnel_domains ADD CONSTRAINT tunnel_domains_live_has_workspace_domain
    CHECK (workspace_domain_id IS NOT NULL OR deleted_at IS NOT NULL) NOT VALID;
ALTER TABLE paperboat.preview_domains ADD CONSTRAINT preview_domains_live_has_workspace_domain
    CHECK (workspace_domain_id IS NOT NULL OR deleted_at IS NOT NULL) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.preview_domains DROP CONSTRAINT preview_domains_live_has_workspace_domain;
ALTER TABLE paperboat.tunnel_domains DROP CONSTRAINT tunnel_domains_live_has_workspace_domain;
DROP TRIGGER bindings_withdraw ON paperboat.workspace_domains;
DROP TRIGGER bindings_follow_state ON paperboat.workspace_domains;
DROP FUNCTION paperboat.workspace_domain_bindings_withdraw();
DROP FUNCTION paperboat.workspace_domain_bindings_follow_state();
