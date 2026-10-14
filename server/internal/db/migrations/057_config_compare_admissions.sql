-- +goose Up
-- +goose StatementBegin
CREATE TABLE paperboat.config_compare_admissions (
 id text PRIMARY KEY,
 operation_id text NOT NULL,
 account_id text NOT NULL REFERENCES paperboat.users(id) ON DELETE CASCADE,
 environment_id text NOT NULL REFERENCES paperboat.control_environments(id) ON DELETE CASCADE,
 machine_id text NOT NULL REFERENCES paperboat.user_machines(id) ON DELETE CASCADE,
 installation_generation bigint NOT NULL CHECK(installation_generation>0),
 assignment_id text NOT NULL REFERENCES paperboat.control_config_assignments(id) ON DELETE CASCADE,
 assignment_version bigint NOT NULL CHECK(assignment_version>0),
 config_path text NOT NULL CHECK(octet_length(config_path) BETWEEN 1 AND 4096),
 conflict_revision text NOT NULL CHECK(octet_length(conflict_revision)=64),
 expected_remote_revision text NOT NULL CHECK(octet_length(expected_remote_revision) BETWEEN 1 AND 256),
 native_cli_session_id text REFERENCES paperboat.cli_client_sessions(id) ON DELETE CASCADE,
 source_machine_id text REFERENCES paperboat.user_machines(id) ON DELETE CASCADE,
 browser_session_id text REFERENCES paperboat.sessions(id) ON DELETE CASCADE,
 browser_key_sha256 text,
 ticket_hash bytea UNIQUE,
 ticket_consumed_at timestamptz,
 workspace_id text NOT NULL,
 origin text, runtime_host text, edge_node_id text, edge_process_epoch text,
 expires_at timestamptz NOT NULL,
 CHECK((native_cli_session_id IS NOT NULL AND source_machine_id IS NOT NULL AND browser_session_id IS NULL AND browser_key_sha256 IS NULL AND ticket_hash IS NULL) OR (native_cli_session_id IS NULL AND source_machine_id IS NULL AND browser_session_id IS NOT NULL AND octet_length(browser_key_sha256)=43 AND octet_length(ticket_hash)=32))
);
CREATE INDEX config_compare_admissions_expiry ON paperboat.config_compare_admissions(expires_at);
CREATE FUNCTION paperboat.snapshot_config_compare_native_usage() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.native_cli_session_id IS NOT NULL THEN
 INSERT INTO paperboat.native_usage_authorities(access_session_id,machine_id,installation_generation,workspace_id,resource_kind,resource_id,resource_name,machine_name,capabilities,admitted_at,expires_at,retain_until)
 SELECT NEW.id,m.id,m.installation_generation,coalesce('team:'||a.adopted_team_id,'personal:'||NEW.account_id),'config',a.id,coalesce(r.display_name,'Configuration')||' config',m.alias,ARRAY['config_compare'],now(),NEW.expires_at,NEW.expires_at+interval '7 days'
 FROM paperboat.control_config_assignments a JOIN paperboat.user_machines m ON m.id=a.machine_id LEFT JOIN paperboat.control_config_repositories r ON r.id=a.repository_id
 WHERE a.id=NEW.assignment_id AND a.version=NEW.assignment_version AND m.id=NEW.machine_id AND m.user_id=NEW.account_id AND m.installation_generation=NEW.installation_generation
 ON CONFLICT(access_session_id) DO NOTHING;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER snapshot_config_compare_native_usage AFTER INSERT ON paperboat.config_compare_admissions FOR EACH ROW EXECUTE FUNCTION paperboat.snapshot_config_compare_native_usage();
-- +goose StatementEnd
-- +goose Down
DROP TABLE paperboat.config_compare_admissions;
DROP FUNCTION paperboat.snapshot_config_compare_native_usage();
