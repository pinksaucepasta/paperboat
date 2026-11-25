-- +goose Up
-- A name a tunnel is moving to stays claimed until its move is settled (419 let a new resource take it once the deadline
-- had passed, while the edges still served it), and a machine service name respects the holds like every other claim.
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.claim_managed_hostname(host text, kind text, resource text, service_port integer DEFAULT 0) RETURNS void
LANGUAGE plpgsql SET search_path TO paperboat,pg_catalog AS $$
DECLARE claimed text;
BEGIN
 IF EXISTS (SELECT 1 FROM tunnel_address_holds h WHERE h.hostname=host AND (h.kind='incoming' OR h.until>now()) AND NOT (claim_managed_hostname.kind='tunnel' AND h.tunnel_id=resource)) THEN
  RAISE EXCEPTION 'managed hostname is already allocated' USING ERRCODE='23505',CONSTRAINT='managed_hostname_reserved';
 END IF;
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
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.reserve_machine_hostname(machine text, service_port integer, candidate text) RETURNS text
LANGUAGE plpgsql SET search_path TO paperboat,pg_catalog AS $$
DECLARE base text := substring(candidate from position('.' in candidate)+1); result text;
BEGIN
 INSERT INTO managed_hostnames(hostname,resource_kind,resource_id,port,domain,machine_id)
 SELECT candidate,'machine',machine,service_port,base,machine
 WHERE NOT EXISTS (SELECT 1 FROM tunnel_address_holds h WHERE h.hostname=candidate AND (h.kind='incoming' OR h.until>now()))
 ON CONFLICT DO NOTHING;
 SELECT hostname INTO result FROM managed_hostnames WHERE resource_kind='machine' AND resource_id=machine AND port=service_port AND domain=base;
 RETURN coalesce(result,'');
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.claim_managed_hostname(host text, kind text, resource text, service_port integer DEFAULT 0) RETURNS void
LANGUAGE plpgsql SET search_path TO paperboat,pg_catalog AS $$
DECLARE claimed text;
BEGIN
 IF EXISTS (SELECT 1 FROM tunnel_address_holds h WHERE h.hostname=host AND h.until>now() AND NOT (claim_managed_hostname.kind='tunnel' AND h.tunnel_id=resource)) THEN
  RAISE EXCEPTION 'managed hostname is already allocated' USING ERRCODE='23505',CONSTRAINT='managed_hostname_reserved';
 END IF;
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
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.reserve_machine_hostname(machine text, service_port integer, candidate text) RETURNS text
LANGUAGE plpgsql SET search_path TO paperboat,pg_catalog AS $$
DECLARE base text := substring(candidate from position('.' in candidate)+1); result text;
BEGIN
 INSERT INTO managed_hostnames(hostname,resource_kind,resource_id,port,domain,machine_id)
 VALUES(candidate,'machine',machine,service_port,base,machine) ON CONFLICT DO NOTHING;
 SELECT hostname INTO result FROM managed_hostnames WHERE resource_kind='machine' AND resource_id=machine AND port=service_port AND domain=base;
 RETURN coalesce(result,'');
END $$;
-- +goose StatementEnd
