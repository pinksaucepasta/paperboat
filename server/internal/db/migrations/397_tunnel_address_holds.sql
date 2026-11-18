-- +goose Up
-- A tunnel's address change is make-before-break: the new name is published to the edges next to the old one
-- ('incoming', until the move times out), and once every live edge serves it the old name is retired and held
-- for a cooldown ('retired', until it may be taken by another resource) so a visitor with the old link cannot
-- land on someone else's tunnel. No foreign key to tunnels: a tunnel is never hard-deleted, the settle worker
-- drops the hold of a deleted one.
SET lock_timeout = '5s';

CREATE TABLE paperboat.tunnel_address_holds (
    hostname text PRIMARY KEY CHECK (hostname = lower(hostname) AND hostname ~ '^[a-z0-9-]+[.][a-z0-9.-]+$'),
    tunnel_id text NOT NULL,
    -- The PATCH operation of an 'incoming' move: the one the settle worker fails if the move is undone.
    operation_id text,
    kind text NOT NULL CHECK (kind IN ('incoming','retired')),
    until timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- One move at a time per tunnel.
CREATE UNIQUE INDEX tunnel_address_holds_incoming ON paperboat.tunnel_address_holds (tunnel_id) WHERE kind = 'incoming';
CREATE INDEX tunnel_address_holds_kind_until ON paperboat.tunnel_address_holds (kind, until);

-- Creating a resource must not mint a name a tunnel is moving to or just left (its cooldown): a visitor with the old
-- link would land on someone else's tunnel. Same function as migration 040 plus the holds check.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.claim_managed_hostname(host text, kind text, resource text, service_port integer DEFAULT 0) RETURNS void
LANGUAGE plpgsql SET search_path TO paperboat,pg_catalog AS $$
DECLARE claimed text;
BEGIN
 IF EXISTS (SELECT 1 FROM tunnel_address_holds h WHERE h.hostname=host AND h.until>now() AND NOT (kind='tunnel' AND h.tunnel_id=resource)) THEN
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

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.claim_managed_hostname(host text, kind text, resource text, service_port integer DEFAULT 0) RETURNS void
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
DROP TABLE paperboat.tunnel_address_holds;
