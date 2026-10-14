-- +goose Up
-- Public labels are independent of opaque runtime identities. Reserve the exact
-- hostname once across durable tunnels, foreground previews and machine ports.
ALTER TABLE paperboat.tunnels DROP CONSTRAINT tunnels_check;
ALTER TABLE paperboat.tunnels ADD CONSTRAINT tunnels_endpoint_url CHECK (stable_endpoint ~ '^https://[a-z0-9-]+[.][a-z0-9.-]+$');
ALTER TABLE paperboat.user_machine_terminal_sessions DROP COLUMN auto_name_ordinal;

CREATE TABLE paperboat.managed_hostnames (
 hostname text PRIMARY KEY CHECK (hostname = lower(hostname) AND hostname ~ '^[a-z0-9-]+[.][a-z0-9.-]+$'),
 resource_kind text NOT NULL CHECK (resource_kind IN ('tunnel','preview','machine')),
 resource_id text NOT NULL,
 port integer NOT NULL DEFAULT 0,
 domain text NOT NULL,
 machine_id text REFERENCES paperboat.user_machines(id) ON DELETE SET NULL,
 CONSTRAINT managed_hostname_owner UNIQUE(resource_kind,resource_id,port,domain),
 CHECK ((resource_kind='machine' AND (machine_id IS NULL OR machine_id=resource_id) AND port BETWEEN 1 AND 65535) OR (resource_kind<>'machine' AND machine_id IS NULL AND port=0))
);

-- +goose StatementBegin
CREATE FUNCTION paperboat.claim_managed_hostname(host text, kind text, resource text, service_port integer DEFAULT 0) RETURNS void
LANGUAGE plpgsql SET search_path TO paperboat,pg_catalog AS $$
DECLARE claimed text;
BEGIN
 INSERT INTO managed_hostnames(hostname,resource_kind,resource_id,port,domain,machine_id)
 VALUES(host,kind,resource,service_port,substring(host from position('.' in host)+1),CASE WHEN kind='machine' THEN resource ELSE NULL END)
 ON CONFLICT(hostname) DO UPDATE SET hostname=EXCLUDED.hostname
 WHERE managed_hostnames.resource_kind=EXCLUDED.resource_kind AND managed_hostnames.resource_id=EXCLUDED.resource_id AND managed_hostnames.port=EXCLUDED.port
 RETURNING hostname INTO claimed;
 IF claimed IS NULL THEN
  RAISE EXCEPTION 'managed hostname is already allocated' USING ERRCODE='23505',CONSTRAINT='managed_hostname_reserved';
 END IF;
END $$;
-- +goose StatementEnd

-- A candidate collision never aborts the surrounding transaction. Concurrent
-- requests for the same machine/port return the already persisted hostname.
-- +goose StatementBegin
CREATE FUNCTION paperboat.reserve_machine_hostname(machine text, service_port integer, candidate text) RETURNS text
LANGUAGE plpgsql SET search_path TO paperboat,pg_catalog AS $$
DECLARE base text := substring(candidate from position('.' in candidate)+1); result text;
BEGIN
 INSERT INTO managed_hostnames(hostname,resource_kind,resource_id,port,domain,machine_id)
 VALUES(candidate,'machine',machine,service_port,base,machine) ON CONFLICT DO NOTHING;
 SELECT hostname INTO result FROM managed_hostnames WHERE resource_kind='machine' AND resource_id=machine AND port=service_port AND domain=base;
 RETURN coalesce(result,'');
END $$;
-- +goose StatementEnd

-- Seed persisted references without regenerating existing names.
SELECT paperboat.claim_managed_hostname(substring(stable_endpoint from 9),'tunnel',id) FROM paperboat.tunnels;
SELECT paperboat.claim_managed_hostname(hostname,'machine',machine_id,substring(target_address from ':([0-9]+)$')::integer) FROM paperboat.lazy_access_policies;
SELECT paperboat.claim_managed_hostname(substring(endpoint from 9),'preview',id) FROM paperboat.preview_leases WHERE owner_session_kind<>'lazy_runtime';

-- +goose StatementBegin
CREATE FUNCTION paperboat.reserve_managed_hostname() RETURNS trigger
LANGUAGE plpgsql SET search_path TO paperboat,pg_catalog AS $$
DECLARE host text; kind text; resource text; service_port integer := 0;
BEGIN
 IF TG_TABLE_NAME='tunnels' THEN
  host := substring(NEW.stable_endpoint from 9); kind := 'tunnel'; resource := NEW.id;
 ELSIF TG_TABLE_NAME='lazy_access_policies' THEN
  host := NEW.hostname; kind := 'machine'; resource := NEW.machine_id;
  service_port := substring(NEW.target_address from ':([0-9]+)$')::integer;
 ELSE
  host := substring(NEW.endpoint from 9); kind := 'preview'; resource := NEW.id;
  IF NEW.owner_session_kind='lazy_runtime' THEN
   SELECT 'machine',p.machine_id,substring(p.target_address from ':([0-9]+)$')::integer INTO kind,resource,service_port
   FROM lazy_access_policies p WHERE p.hostname=host AND p.machine_id=NEW.owner_machine_id AND p.account_id=NEW.account_id AND p.target_address=NEW.target_address AND p.target_scheme=NEW.target_scheme AND p.deleted_at IS NULL;
   IF NOT FOUND THEN RAISE EXCEPTION 'preview hostname has no exact lazy policy' USING ERRCODE='23514'; END IF;
  END IF;
 END IF;
 PERFORM claim_managed_hostname(host,kind,resource,service_port);
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER tunnel_hostname_reservation BEFORE INSERT ON paperboat.tunnels FOR EACH ROW EXECUTE FUNCTION paperboat.reserve_managed_hostname();
CREATE TRIGGER preview_hostname_reservation BEFORE INSERT ON paperboat.preview_leases FOR EACH ROW EXECUTE FUNCTION paperboat.reserve_managed_hostname();
CREATE TRIGGER lazy_hostname_reservation BEFORE INSERT ON paperboat.lazy_access_policies FOR EACH ROW EXECUTE FUNCTION paperboat.reserve_managed_hostname();

-- Foreground names remain reserved for their retained history. Lazy leases use
-- the machine/port reservation and can activate again after stopping.
DROP INDEX paperboat.preview_leases_endpoint_reserved;
CREATE UNIQUE INDEX preview_leases_endpoint_reserved ON paperboat.preview_leases(endpoint) WHERE terminal_state='active' OR owner_session_kind<>'lazy_runtime';

-- Reservations are intentional tombstones after source-history deletion. This
-- prevents an old public URL from being assigned to another resource/account.
DROP TRIGGER machine_environment_label ON paperboat.user_machines;
DROP FUNCTION paperboat.ensure_machine_environment_label();
DROP TABLE paperboat.lazy_environment_identities;

-- +goose Down
-- Public name allocation is a v1 source cutover; use release rollback rather
-- than deriving retired random environment labels from current hostnames.
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'readable naming migration requires release rollback'; END $$;
-- +goose StatementEnd
