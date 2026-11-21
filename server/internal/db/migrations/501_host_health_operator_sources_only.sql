-- +goose Up
-- Edge certificate renewal now rides in each edge's authenticated heartbeat (control_tunnel_nodes.observation); this
-- table keeps only the operator-side reports, written through the operator API. 502 narrows the check (NOT VALID) and
-- 503 validates it.
SET LOCAL lock_timeout = '5s';
DELETE FROM paperboat.host_health_reports WHERE source NOT IN ('hosts', 'canary', 'deploy');

-- +goose Down
SELECT 1;
