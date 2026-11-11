-- +goose Up
SET LOCAL lock_timeout = '5s';
-- Requests and connections an edge refused for flood protection (a client address over its limit,
-- or one name over its share of the edge), per hostname, reporting edge and hour: the owner's signal
-- that Paperboat limited their link. Written from edge heartbeats; rows older than 7 days are deleted
-- by the same writer, so the table stays bounded.
CREATE TABLE paperboat.edge_refusals (
    hostname text NOT NULL CHECK (char_length(hostname) BETWEEN 1 AND 253),
    edge_node_id text NOT NULL,
    hour timestamptz NOT NULL,
    refused bigint NOT NULL CHECK (refused > 0),
    PRIMARY KEY (hostname, edge_node_id, hour)
);
CREATE INDEX edge_refusals_hour ON paperboat.edge_refusals(hour);

-- +goose Down
DROP TABLE paperboat.edge_refusals;
