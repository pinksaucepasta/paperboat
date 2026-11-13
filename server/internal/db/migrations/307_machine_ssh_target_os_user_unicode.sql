-- +goose Up
SET LOCAL lock_timeout = '5s';
-- A Windows account such as "Zoë Test" is a valid managed SSH target (the Go rule managedssh.ValidOSUser is the
-- gate): the baseline check refused every space, so pairing such a machine failed with HTTP 500 (X-28). Control
-- characters, '@', a leading '-' and edge spaces stay refused here. Added NOT VALID and validated in 308.
ALTER TABLE paperboat.machine_ssh_targets
    DROP CONSTRAINT machine_ssh_targets_os_user_check,
    ADD CONSTRAINT machine_ssh_targets_os_user_check CHECK (octet_length(os_user) BETWEEN 1 AND 255 AND os_user = btrim(os_user) AND os_user !~ '[[:cntrl:]@]' AND left(os_user, 1) <> '-') NOT VALID;

-- +goose Down
SELECT 1;
