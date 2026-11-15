-- +goose Up
SET LOCAL lock_timeout = '5s';
-- Browser names for machine ports (<name>.<machine>.<domain>, and top-level <name>.<domain>), stored on the server
-- so every computer allowed the port sees them. A name never grants access: clients receive only names of ports
-- the viewer is already authorized for. scope 'machine' names one machine's port; 'all_machines' names that port on
-- every machine of a workspace (a machine name of the same label wins); 'top_level' maps <name>.<domain> to one
-- machine's port (a machine with that name wins). Workspace: owner_team_id, or owner_account_id for personal.
CREATE TABLE paperboat.machine_port_names (
    id text PRIMARY KEY DEFAULT ('pn_' || replace(gen_random_uuid()::text, '-', '')),
    scope text NOT NULL CHECK (scope IN ('machine', 'all_machines', 'top_level')),
    name text NOT NULL CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$' AND name !~ '^[0-9]+$'),
    port integer NOT NULL CHECK (port BETWEEN 1 AND 65535),
    machine_id text REFERENCES paperboat.user_machines(id) ON DELETE CASCADE,
    owner_account_id text REFERENCES paperboat.users(id) ON DELETE CASCADE,
    owner_team_id text REFERENCES paperboat.teams(team_id) ON DELETE CASCADE,
    created_by text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((scope = 'machine') = (owner_account_id IS NULL AND owner_team_id IS NULL)),
    CHECK (scope = 'machine' OR (owner_account_id IS NULL) <> (owner_team_id IS NULL)),
    CHECK ((scope = 'all_machines') = (machine_id IS NULL))
);
CREATE UNIQUE INDEX machine_port_names_machine ON paperboat.machine_port_names(machine_id, name) WHERE scope = 'machine';
CREATE UNIQUE INDEX machine_port_names_workspace ON paperboat.machine_port_names(scope, coalesce(owner_team_id, owner_account_id), name) WHERE scope <> 'machine';
CREATE INDEX machine_port_names_target ON paperboat.machine_port_names(machine_id) WHERE machine_id IS NOT NULL;

-- +goose Down
DROP TABLE paperboat.machine_port_names;
