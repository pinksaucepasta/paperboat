-- +goose NO TRANSACTION
-- +goose Up
-- Replacements for tunnel_domains_tunnel_state and preview_domains_preview_state, which lead with the binding
-- ownership_state copy that 500/501 drop (dropping the column drops them): per-tunnel and per-preview binding
-- lists keep an index. tunnel_domains_verification and the two *_dns_due indexes go with the column; nothing reads them.
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS paperboat.tunnel_domains_tunnel;
CREATE INDEX CONCURRENTLY tunnel_domains_tunnel ON paperboat.tunnel_domains (tunnel_id, certificate_state, hostname, id);
DROP INDEX CONCURRENTLY IF EXISTS paperboat.preview_domains_preview;
CREATE INDEX CONCURRENTLY preview_domains_preview ON paperboat.preview_domains (preview_id, certificate_state, hostname, id);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS paperboat.tunnel_domains_tunnel;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.preview_domains_preview;
