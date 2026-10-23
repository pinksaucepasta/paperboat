-- +goose Up
-- The latest host-key observation of a machine that was refused because another live machine
-- owns the same keys (an image cloned with its /etc/ssh keys). Shown as ssh.host_key_conflict in
-- the machine list; removed when an observation succeeds. A separate table, so recording a
-- refusal never takes a lock on the hot machine_ssh_targets rows.
CREATE TABLE paperboat.machine_ssh_host_key_conflicts (
    user_machine_id text PRIMARY KEY REFERENCES paperboat.user_machines(id) ON DELETE CASCADE,
    other_machine_id text NOT NULL REFERENCES paperboat.user_machines(id) ON DELETE CASCADE,
    observed_at timestamptz NOT NULL
);
CREATE INDEX machine_ssh_host_key_conflicts_other ON paperboat.machine_ssh_host_key_conflicts (other_machine_id);

-- +goose Down
DROP TABLE paperboat.machine_ssh_host_key_conflicts;
