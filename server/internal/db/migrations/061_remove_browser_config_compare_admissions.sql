-- +goose Up
-- Retire browser comparison tickets while preserving native CLI admissions.
DELETE FROM paperboat.config_compare_admissions WHERE native_cli_session_id IS NULL;

ALTER TABLE paperboat.config_compare_admissions
 DROP CONSTRAINT config_compare_admissions_check;

ALTER TABLE paperboat.config_compare_admissions
 ALTER COLUMN native_cli_session_id SET NOT NULL,
 ALTER COLUMN source_machine_id SET NOT NULL,
 DROP COLUMN browser_session_id,
 DROP COLUMN browser_key_sha256,
 DROP COLUMN ticket_hash,
 DROP COLUMN ticket_consumed_at,
 DROP COLUMN origin,
 DROP COLUMN runtime_host,
 DROP COLUMN edge_node_id,
 DROP COLUMN edge_process_epoch;

-- +goose Down
ALTER TABLE paperboat.config_compare_admissions
 ADD COLUMN browser_session_id text REFERENCES paperboat.sessions(id) ON DELETE CASCADE,
 ADD COLUMN browser_key_sha256 text,
 ADD COLUMN ticket_hash bytea UNIQUE,
 ADD COLUMN ticket_consumed_at timestamptz,
 ADD COLUMN origin text,
 ADD COLUMN runtime_host text,
 ADD COLUMN edge_node_id text,
 ADD COLUMN edge_process_epoch text,
 ALTER COLUMN native_cli_session_id DROP NOT NULL,
 ALTER COLUMN source_machine_id DROP NOT NULL,
 ADD CONSTRAINT config_compare_admissions_check CHECK((native_cli_session_id IS NOT NULL AND source_machine_id IS NOT NULL AND browser_session_id IS NULL AND browser_key_sha256 IS NULL AND ticket_hash IS NULL) OR (native_cli_session_id IS NULL AND source_machine_id IS NULL AND browser_session_id IS NOT NULL AND octet_length(browser_key_sha256)=43 AND octet_length(ticket_hash)=32));
