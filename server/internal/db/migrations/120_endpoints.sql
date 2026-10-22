-- +goose Up
-- PLAN 3a: one endpoint engine for previews and tunnels. An endpoint is served by its agent (the
-- enrolled machine's daemon) over the runtime carriers that agent already holds to each edge of its
-- workspace's edge set; an endpoint never opens a carrier of its own.

CREATE TABLE paperboat.endpoints (
    id text PRIMARY KEY CHECK (id ~ '^[A-Za-z0-9_-]{3,128}$'),
    kind text NOT NULL CHECK (kind IN ('preview', 'tunnel')),
    account_id text NOT NULL REFERENCES paperboat.users(id) ON DELETE CASCADE,
    owner_team_id text REFERENCES paperboat.teams(team_id),
    agent_machine_id text NOT NULL REFERENCES paperboat.user_machines(id) ON DELETE CASCADE,
    access_mode text NOT NULL CHECK (access_mode IN ('public', 'private', 'team')),
    lifetime text NOT NULL CHECK (lifetime IN ('lease', 'persistent')),
    -- A preview's lease (deadline, renewal, stop) stays owned by preview_leases until the old
    -- per-preview carrier path is deleted (3a step 5); the engine reads it from there.
    preview_lease_id text UNIQUE REFERENCES paperboat.preview_leases(id) ON DELETE CASCADE,
    desired_state text NOT NULL DEFAULT 'active' CHECK (desired_state IN ('active', 'removed')),
    generation bigint NOT NULL DEFAULT 1 CHECK (generation > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((kind = 'preview') = (lifetime = 'lease')),
    CHECK ((kind = 'preview') = (preview_lease_id IS NOT NULL))
);
CREATE INDEX endpoints_agent ON paperboat.endpoints(agent_machine_id) WHERE desired_state = 'active';

CREATE TABLE paperboat.endpoint_routes (
    id text PRIMARY KEY CHECK (id ~ '^[A-Za-z0-9_-]{3,128}$'),
    endpoint_id text NOT NULL REFERENCES paperboat.endpoints(id) ON DELETE CASCADE,
    hostname text NOT NULL CHECK (hostname ~ '^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$'),
    protocol text NOT NULL CHECK (protocol IN ('http', 'ws', 'tcp', 'tls', 'private_tcp')),
    revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0)
);
CREATE INDEX endpoint_routes_endpoint ON paperboat.endpoint_routes(endpoint_id);

-- One row per (route, edge): the route is served on that edge over the agent's runtime carrier
-- session. desired is written by the reconciler, observed_* by the edge.
CREATE TABLE paperboat.endpoint_edge_assignments (
    route_id text NOT NULL REFERENCES paperboat.endpoint_routes(id) ON DELETE CASCADE,
    endpoint_id text NOT NULL REFERENCES paperboat.endpoints(id) ON DELETE CASCADE,
    edge_node_id text NOT NULL,
    edge_process_epoch text NOT NULL,
    carrier_session_id text NOT NULL,
    generation bigint NOT NULL DEFAULT 1 CHECK (generation > 0),
    desired text NOT NULL DEFAULT 'active' CHECK (desired IN ('active', 'removed')),
    observed_ready boolean NOT NULL DEFAULT false,
    observed_at timestamptz,
    PRIMARY KEY (route_id, edge_node_id),
    CHECK (desired = 'active' OR NOT observed_ready)
);
CREATE INDEX endpoint_edge_assignments_edge ON paperboat.endpoint_edge_assignments(edge_node_id) WHERE desired = 'active';
CREATE INDEX endpoint_edge_assignments_endpoint ON paperboat.endpoint_edge_assignments(endpoint_id);

-- What the agent behind a runtime carrier can serve. 'endpoints' = streams for endpoint routes.
ALTER TABLE paperboat.runtime_carrier_attachments
    ADD COLUMN agent_capabilities text[] NOT NULL DEFAULT '{}'
    CHECK (agent_capabilities <@ ARRAY['endpoints']::text[]);

-- +goose Down
ALTER TABLE paperboat.runtime_carrier_attachments DROP COLUMN agent_capabilities;
DROP TABLE paperboat.endpoint_edge_assignments;
DROP TABLE paperboat.endpoint_routes;
DROP TABLE paperboat.endpoints;
