-- +goose Up
SET LOCAL lock_timeout = '5s';
-- Every new database connection planned the expanded inspector authorization view on its first configuration:
-- ~75-115 ms of planner time on prod (EXPLAIN SUMMARY 11-19), the same query then takes 0.7 ms once the connection
-- caches the generic plan. The time is the join-order search over the view's five joins and four EXISTS branches:
-- with join_collapse_limit=1 the same statement plans in ~6 ms. The view is written in the right order already (the
-- credential row selected by endpoint and account, then its session, machine and accounts by primary key).

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.peer_network_inspector_admissions(
 wanted_endpoint text, wanted_account text, at_time timestamptz, admitted text[])
 RETURNS TABLE(credential_id text, native_cli_session_id text, native_machine_id text,
 account_id text, owner_account_id text, expires_at timestamptz)
 LANGUAGE plpgsql STABLE
 SET plan_cache_mode TO 'force_generic_plan'
 SET join_collapse_limit TO 1
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

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION '459 is forward-only: restore peer_network_inspector_admissions from migration 361'; END $$;
-- +goose StatementEnd
