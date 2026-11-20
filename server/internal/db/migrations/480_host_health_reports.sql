-- +goose Up
SET LOCAL lock_timeout = '5s';
-- The production canary runner pushes its latest host-capacity and canary state here for the admin panel: one row per source,
-- overwritten by each push, so the table never grows.
CREATE TABLE paperboat.host_health_reports (
    source text PRIMARY KEY CHECK (source IN ('hosts', 'canary')),
    payload jsonb NOT NULL,
    reported_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE paperboat.host_health_reports;
