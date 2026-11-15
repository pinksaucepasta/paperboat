-- +goose Up
SET LOCAL lock_timeout = '5s';
-- The front-desk port of a machine: where an unmatched <name>.<machine>.<domain> goes (a reverse proxy such as
-- Coolify, Traefik or Caddy). No row means port 80 over http.
CREATE TABLE paperboat.machine_front_desks (
    machine_id text PRIMARY KEY REFERENCES paperboat.user_machines(id) ON DELETE CASCADE,
    port integer NOT NULL CHECK (port BETWEEN 1 AND 65535),
    scheme text NOT NULL CHECK (scheme IN ('http', 'https')),
    updated_by text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE paperboat.machine_front_desks;
