-- +goose Up
SET LOCAL lock_timeout = '5s';
-- When each machine last fetched the CA set it trusts (managedssh UserCAs). Break-glass publishes a pending CA and
-- reads this to show which machines hold it before any certificate is signed with it. One row per machine, removed
-- with the machine.
CREATE TABLE paperboat.managed_ssh_ca_fetches (
    machine_id text PRIMARY KEY REFERENCES paperboat.user_machines(id) ON DELETE CASCADE,
    fetched_at timestamptz NOT NULL
);

-- +goose Down
DROP TABLE paperboat.managed_ssh_ca_fetches;
