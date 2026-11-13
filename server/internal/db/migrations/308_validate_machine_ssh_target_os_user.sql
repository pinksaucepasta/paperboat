-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.machine_ssh_targets VALIDATE CONSTRAINT machine_ssh_targets_os_user_check;

-- +goose Down
SELECT 1;
