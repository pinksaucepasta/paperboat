-- +goose Up
-- Quiesce managed SSH clients before cutover; resume from authoritative current
-- state with fresh operations. Historical request hashes and audit stay exact.
LOCK TABLE paperboat.machine_ssh_host_key_sets, paperboat.machine_ssh_host_keys,
 paperboat.managed_ssh_operations IN ACCESS EXCLUSIVE MODE;
CREATE TEMP TABLE ssh_keyset_identity_rekeys ON COMMIT DROP AS
 SELECT id AS old_id, 'keyset_' || pg_catalog.gen_random_uuid()::text AS new_id,
 user_machine_id
 FROM paperboat.machine_ssh_host_key_sets
 WHERE id !~ '^keyset_[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$';
ALTER TABLE paperboat.machine_ssh_host_keys DROP CONSTRAINT machine_ssh_host_keys_set_id_user_machine_id_fkey;
ALTER TABLE paperboat.machine_ssh_host_key_sets DROP CONSTRAINT machine_ssh_host_key_sets_id_check;
UPDATE paperboat.machine_ssh_host_key_sets s SET id=r.new_id
 FROM ssh_keyset_identity_rekeys r WHERE s.id=r.old_id;
UPDATE paperboat.machine_ssh_host_keys k SET set_id=r.new_id
 FROM ssh_keyset_identity_rekeys r WHERE k.set_id=r.old_id AND k.user_machine_id=r.user_machine_id;
UPDATE paperboat.managed_ssh_operations o SET resource_id=r.new_id
 FROM ssh_keyset_identity_rekeys r WHERE o.resource_id=r.old_id
 AND o.operation_kind IN ('host_keys_observe','host_keys_promote');
ALTER TABLE paperboat.machine_ssh_host_keys ADD CONSTRAINT machine_ssh_host_keys_set_id_user_machine_id_fkey
 FOREIGN KEY (set_id,user_machine_id) REFERENCES paperboat.machine_ssh_host_key_sets(id,user_machine_id) ON DELETE CASCADE;
ALTER TABLE paperboat.machine_ssh_host_key_sets ADD CONSTRAINT machine_ssh_host_key_sets_id_check
 CHECK (id ~ '^keyset_[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$');
INSERT INTO paperboat.audit_events(id,actor_type,event_type,resource_type,resource_id,
 account_id,change_type,outcome,metadata)
 SELECT 'event_' || pg_catalog.gen_random_uuid()::text,'system','managed_ssh.host_key_set_identity_rekeyed',
 'machine_ssh_host_key_set',r.new_id,m.user_id,'migration','changed',
 jsonb_build_object('old_set_id',r.old_id,'new_set_id',r.new_id,'user_machine_id',r.user_machine_id)
 FROM ssh_keyset_identity_rekeys r JOIN paperboat.user_machines m ON m.id=r.user_machine_id;
DROP TABLE ssh_keyset_identity_rekeys;
