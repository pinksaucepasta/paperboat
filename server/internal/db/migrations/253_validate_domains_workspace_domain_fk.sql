-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.tunnel_domains VALIDATE CONSTRAINT tunnel_domains_workspace_domain_fk;
ALTER TABLE paperboat.preview_domains VALIDATE CONSTRAINT preview_domains_workspace_domain_fk;

-- +goose Down
SELECT 1;
