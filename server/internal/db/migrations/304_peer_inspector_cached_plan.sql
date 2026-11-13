-- +goose Up
-- Planning the expanded inspector view cost 120-424 ms per configuration request on prod,
-- even with no credentials. Keep its plan in PL/pgSQL, as migration 296 does for edge policy.
-- STABLE keeps every authorization check on the caller's snapshot; no authority is cached.
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
CREATE FUNCTION paperboat.peer_network_inspector_admissions(
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

-- +goose Down
DROP FUNCTION paperboat.peer_network_inspector_admissions(text,text,timestamptz,text[]);
