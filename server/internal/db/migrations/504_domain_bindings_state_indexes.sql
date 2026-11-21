-- +goose NO TRANSACTION
-- +goose Up
-- 505/506 drop the binding ownership_state copy. The indexes that lead with it (tunnel_domains_tunnel_state,
-- preview_domains_preview_state) are replaced here by per-binding indexes without it, and dropped concurrently
-- with tunnel_domains_verification and the two *_dns_due indexes, so 505/506 drop no index under their lock.
-- No code names any of these indexes. IF EXISTS on the new names clears an INVALID leftover on a rerun.
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS paperboat.tunnel_domains_tunnel;
CREATE INDEX CONCURRENTLY tunnel_domains_tunnel ON paperboat.tunnel_domains (tunnel_id, certificate_state, hostname, id);
DROP INDEX CONCURRENTLY IF EXISTS paperboat.preview_domains_preview;
CREATE INDEX CONCURRENTLY preview_domains_preview ON paperboat.preview_domains (preview_id, certificate_state, hostname, id);
DROP INDEX CONCURRENTLY IF EXISTS paperboat.tunnel_domains_tunnel_state;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.preview_domains_preview_state;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.tunnel_domains_verification;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.tunnel_domains_dns_due;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.preview_domains_dns_due;

-- +goose Down
-- Forward only for the dropped ones (they go with the column in 505/506).
DROP INDEX CONCURRENTLY IF EXISTS paperboat.tunnel_domains_tunnel;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.preview_domains_preview;
