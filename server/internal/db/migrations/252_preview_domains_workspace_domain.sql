-- +goose Up
-- A preview hostname lives under a workspace domain and reuses its verification and certificate.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.preview_domains ADD COLUMN workspace_domain_id text;
ALTER TABLE paperboat.preview_domains ADD CONSTRAINT preview_domains_workspace_domain_fk
    FOREIGN KEY (workspace_domain_id) REFERENCES paperboat.workspace_domains(id) ON DELETE SET NULL NOT VALID;
CREATE INDEX preview_domains_workspace_domain ON paperboat.preview_domains (workspace_domain_id) WHERE deleted_at IS NULL;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.preview_domains DROP COLUMN workspace_domain_id;
