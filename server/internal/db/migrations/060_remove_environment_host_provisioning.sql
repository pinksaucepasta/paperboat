-- +goose Up
DROP TABLE paperboat.environment_vault_projection_sources;
DROP TABLE paperboat.environment_vault_projection_fences;
ALTER TABLE paperboat.environment_vault_hosts
 DROP COLUMN writer_public,
 DROP COLUMN selection_generation,
 DROP COLUMN selection,
 DROP COLUMN projection_revision,
 DROP COLUMN document_id,
 DROP COLUMN envelope,
 DROP COLUMN applied_fence_generation,
 DROP COLUMN observation_seq,
 DROP COLUMN observation_digest,
 DROP COLUMN observation;

-- +goose Down
-- Removed encrypted projections and source selections cannot be reconstructed
-- from new independent encrypted layers without endpoint keys. Refuse rather
-- than restoring empty metadata that would silently alter execution readiness.
-- +goose StatementBegin
DO $$ BEGIN
 RAISE EXCEPTION 'automatic encrypted ENV layers cannot downgrade to removed manual provisioning';
END $$;
-- +goose StatementEnd
