-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.tunnel_domains VALIDATE CONSTRAINT tunnel_domains_live_has_workspace_domain;
ALTER TABLE paperboat.preview_domains VALIDATE CONSTRAINT preview_domains_live_has_workspace_domain;

-- +goose Down
SELECT 1;
