-- +goose Up
-- A team can require an owner or admin to accept machines that members add to it. approve_new_machines
-- is that setting (on by default). While a machine waits it has no team binding; a request row holds it
-- (pending), or records the refusal (declined) so its owner sees the answer.
ALTER TABLE paperboat.teams ADD COLUMN approve_new_machines boolean NOT NULL DEFAULT true;

CREATE TABLE paperboat.team_machine_requests (
    team_id text NOT NULL REFERENCES paperboat.teams(team_id) ON DELETE CASCADE,
    machine_id text NOT NULL REFERENCES paperboat.user_machines(id) ON DELETE CASCADE,
    requested_by text NOT NULL REFERENCES paperboat.users(id) ON DELETE CASCADE,
    state text NOT NULL CHECK (state IN ('pending', 'declined')),
    requested_at timestamptz NOT NULL DEFAULT now(),
    decided_by text REFERENCES paperboat.users(id) ON DELETE SET NULL,
    decided_at timestamptz,
    PRIMARY KEY (team_id, machine_id)
);
CREATE INDEX team_machine_requests_requester ON paperboat.team_machine_requests (requested_by);

-- +goose Down
DROP TABLE paperboat.team_machine_requests;
ALTER TABLE paperboat.teams DROP COLUMN approve_new_machines;
