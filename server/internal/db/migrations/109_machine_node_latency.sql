-- +goose Up
-- The latest connect time a machine measured to each edge or relay it can use. Display data for the
-- console and CLI: it never selects a path and never confers access. One row per machine and node.
CREATE TABLE paperboat.machine_node_latency (
    machine_id  text        NOT NULL REFERENCES paperboat.user_machines(id) ON DELETE CASCADE,
    node_id     text        NOT NULL CHECK (length(node_id) BETWEEN 1 AND 128),
    role        text        NOT NULL CHECK (role IN ('edge', 'relay')),
    rtt_ms      integer     NOT NULL CHECK (rtt_ms BETWEEN 0 AND 10000),
    measured_at timestamptz NOT NULL,
    PRIMARY KEY (machine_id, node_id)
);

-- +goose Down
DROP TABLE paperboat.machine_node_latency;
