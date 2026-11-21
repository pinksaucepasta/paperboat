-- +goose Up
-- small-table: pre-launch, a handful of rows. A tunnel or preview hostname is served only while the workspace
-- domain it was bound through is verified; bindings without one are withdrawn, the rest take its state.
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);
UPDATE paperboat.tunnel_domains SET ownership_state = 'revoked',
    certificate_state = CASE WHEN certificate_state = 'not_applicable' THEN 'not_applicable' ELSE 'revoked' END,
    conflict_state = 'quarantined', quarantine_until = now(), -- already over: the next owner may bind at once
        deleted_at = now(), generation = generation + 1, updated_at = now()
WHERE deleted_at IS NULL AND workspace_domain_id IS NULL;
UPDATE paperboat.preview_domains SET ownership_state = 'revoked',
    certificate_state = CASE WHEN certificate_state = 'not_applicable' THEN 'not_applicable' ELSE 'revoked' END,
    conflict_state = 'quarantined', quarantine_until = now(), -- already over: the next owner may bind at once
        deleted_at = now(), generation = generation + 1, updated_at = now()
WHERE deleted_at IS NULL AND workspace_domain_id IS NULL;
UPDATE paperboat.tunnel_domains d SET ownership_state = 'failed', generation = d.generation + 1, updated_at = now()
FROM paperboat.workspace_domains w
WHERE d.workspace_domain_id = w.id AND d.deleted_at IS NULL AND w.ownership_state <> 'verified' AND d.ownership_state = 'verified';
UPDATE paperboat.preview_domains d SET ownership_state = 'failed', generation = d.generation + 1, updated_at = now()
FROM paperboat.workspace_domains w
WHERE d.workspace_domain_id = w.id AND d.deleted_at IS NULL AND w.ownership_state <> 'verified' AND d.ownership_state = 'verified';

-- +goose Down
SELECT 1;
