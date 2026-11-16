-- +goose Up
SET LOCAL lock_timeout = '5s';
-- Inspector access is tied to its preview's lease or its tunnel, but the peer policy cache (peersessions/policy_cache.go)
-- kept a document holding it until the credential expired (up to 5 min) when the lease ended first: no trigger named the
-- parties on a lease or tunnel change, and the grant's expiry ignored the lease deadline. Now the grant ends at the
-- earliest of the credential, the lease (and its user deadline) or the tunnel expiry, and stopping or shortening a lease,
-- or changing a tunnel or its route, notifies the credential's endpoint and machine (as publish_peer_policy_account_row
-- does for inspector_credentials). A lease renewal (lease_deadline extended) notifies nothing: the grant's expiry already
-- bounds the document, which is rebuilt then. Preview routes (a view over edge assignments) are heartbeat state that
-- lapses with time, not a write to trigger on.
-- The triggers that call publish_peer_policy_inspected are 362-364, one table each.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.peer_network_inspector_admissions(
 wanted_endpoint text, wanted_account text, at_time timestamptz, admitted text[])
 RETURNS TABLE(credential_id text, native_cli_session_id text, native_machine_id text,
 account_id text, owner_account_id text, expires_at timestamptz)
 LANGUAGE plpgsql STABLE
 SET plan_cache_mode TO 'force_generic_plan'
AS $function$
BEGIN
 RETURN QUERY
 SELECT i.credential_id,i.native_cli_session_id,i.native_machine_id,
        i.account_id,i.owner_account_id,
        least(i.expires_at,
          (SELECT least(p.lease_deadline, p.user_deadline) FROM paperboat.preview_leases p
           WHERE c.resource_kind='preview' AND p.id=c.resource_id),
          (SELECT t.expires_at FROM paperboat.tunnels t WHERE c.resource_kind='tunnel' AND t.id=c.resource_id))
 FROM paperboat.inspector_native_admissions i
 JOIN paperboat.inspector_credentials c ON c.credential_id=i.credential_id
 WHERE ((i.native_cli_session_id=wanted_endpoint AND i.account_id=wanted_account)
     OR (i.native_machine_id=wanted_endpoint AND i.owner_account_id=wanted_account))
 AND (i.expires_at>at_time OR i.credential_id=ANY(admitted))
 ORDER BY (i.credential_id=ANY(admitted)) DESC,i.credential_id LIMIT 257;
END
$function$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_peer_policy_inspected() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE kind text; ids text[];
BEGIN
  CASE TG_TABLE_NAME
  WHEN 'preview_leases' THEN kind := 'preview'; ids := ARRAY[coalesce(NEW.id, OLD.id)];
  WHEN 'tunnels' THEN kind := 'tunnel'; ids := ARRAY[coalesce(NEW.id, OLD.id)];
  WHEN 'tunnel_routes' THEN kind := 'tunnel'; ids := ARRAY[NEW.tunnel_id, OLD.tunnel_id];
  END CASE;
  PERFORM paperboat.publish_peer_policy(ARRAY(
    SELECT k FROM paperboat.inspector_credentials c, LATERAL unnest(ARRAY[c.native_machine_id, c.native_cli_session_id]) k
    WHERE c.resource_id = ANY (ids) AND c.resource_kind = kind AND c.revoked_at IS NULL));
  RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP FUNCTION paperboat.publish_peer_policy_inspected();
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.peer_network_inspector_admissions(
 wanted_endpoint text, wanted_account text, at_time timestamptz, admitted text[])
 RETURNS TABLE(credential_id text, native_cli_session_id text, native_machine_id text,
 account_id text, owner_account_id text, expires_at timestamptz)
 LANGUAGE plpgsql STABLE
 SET plan_cache_mode TO 'force_generic_plan'
AS $function$
BEGIN
 RETURN QUERY
 SELECT i.credential_id,i.native_cli_session_id,i.native_machine_id,
        i.account_id,i.owner_account_id,i.expires_at
 FROM paperboat.inspector_native_admissions i
 WHERE ((i.native_cli_session_id=wanted_endpoint AND i.account_id=wanted_account)
     OR (i.native_machine_id=wanted_endpoint AND i.owner_account_id=wanted_account))
 AND (i.expires_at>at_time OR i.credential_id=ANY(admitted))
 ORDER BY (i.credential_id=ANY(admitted)) DESC,i.credential_id LIMIT 257;
END
$function$;
-- +goose StatementEnd
