-- +goose Up
-- What each edge, relay and the control-plane server reports about itself (nodemetrics.Report): the latest sample and
-- cumulative byte counters per process epoch, and one row per node and UTC day with the bytes carried and the peaks.
-- No foreign key to control_tunnel_nodes: the server records itself here too, and a node's history outlives its row.
SET lock_timeout = '5s';

CREATE TABLE paperboat.node_capacity (
    node_id text PRIMARY KEY,
    role text NOT NULL CHECK (role IN ('edge','relay','server')),
    process_epoch text NOT NULL,
    version text NOT NULL DEFAULT '',
    cpu_percent real NOT NULL DEFAULT 0,
    -- Exponentially weighted CPU over the sustained window: "sustained" warnings read this, not one sample.
    cpu_sustained_percent real NOT NULL DEFAULT 0,
    rss_bytes bigint NOT NULL DEFAULT 0,
    connections integer,
    bytes_in_total bigint NOT NULL DEFAULT 0,
    bytes_out_total bigint NOT NULL DEFAULT 0,
    reported_at timestamptz NOT NULL
);

CREATE TABLE paperboat.node_capacity_daily (
    node_id text NOT NULL,
    day date NOT NULL,
    bytes_in bigint NOT NULL DEFAULT 0,
    bytes_out bigint NOT NULL DEFAULT 0,
    cpu_max real NOT NULL DEFAULT 0,
    rss_max bigint NOT NULL DEFAULT 0,
    connections_max integer NOT NULL DEFAULT 0,
    PRIMARY KEY (node_id, day)
);
CREATE INDEX node_capacity_daily_day ON paperboat.node_capacity_daily (day);

-- +goose Down
DROP TABLE paperboat.node_capacity_daily;
DROP TABLE paperboat.node_capacity;
