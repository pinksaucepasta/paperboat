-- +goose Up
-- F18-06: an API token used with pb becomes a token-backed CLI session. The session carries the
-- token; it authenticates only while the token is live, and every machine authorization of it
-- also checks the token's scope. The scope is stored like policy rules: machines by ID, tags and
-- every machine by name, resolved per request against the workspace's policy tags.
ALTER TABLE paperboat.cli_client_sessions ADD COLUMN api_token_id text REFERENCES paperboat.api_tokens(id);
CREATE INDEX cli_client_sessions_api_token ON paperboat.cli_client_sessions(api_token_id) WHERE api_token_id IS NOT NULL;

CREATE TABLE paperboat.api_token_scope_rules (
    token_id text NOT NULL REFERENCES paperboat.api_tokens(id) ON DELETE CASCADE,
    entry integer NOT NULL CHECK (entry >= 0),
    target_kind text NOT NULL CHECK (target_kind IN ('all_machines', 'machine', 'tag', 'preview', 'tunnel', 'env')),
    target_id text NOT NULL,
    action text NOT NULL,
    ports int4range NOT NULL
);
CREATE INDEX api_token_scope_rules_lookup ON paperboat.api_token_scope_rules(token_id, action);

-- Whether a token's scope covers one machine action (port NULL: any port). A token without a
-- scope covers everything its creator may do; a scoped token created before its rows existed
-- has none and covers nothing.
-- +goose StatementBegin
CREATE FUNCTION paperboat.api_token_scope_allows(token text, machine text, capability text, port integer) RETURNS boolean
    LANGUAGE sql STABLE
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
 SELECT EXISTS (
  SELECT 1 FROM api_tokens t
  WHERE t.id = token AND t.revoked_at IS NULL AND t.expires_at > now()
   AND (t.scope IS NULL OR EXISTS (
    SELECT 1 FROM api_token_scope_rules r
    LEFT JOIN workspace_policies p ON (t.team_id IS NOT NULL AND p.team_id = t.team_id) OR (t.team_id IS NULL AND p.owner_account = t.account_id)
    WHERE r.token_id = t.id AND r.action = capability AND (port IS NULL OR r.ports @> port)
     AND (r.target_kind = 'all_machines'
       OR (r.target_kind = 'machine' AND r.target_id = machine)
       OR (r.target_kind = 'tag' AND EXISTS(SELECT 1 FROM workspace_policy_tags tg WHERE tg.policy_id = p.id AND tg.tag = r.target_id AND tg.machine_id = machine))))));
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION paperboat.api_token_scope_allows(text, text, text, integer);
DROP TABLE paperboat.api_token_scope_rules;
ALTER TABLE paperboat.cli_client_sessions DROP COLUMN api_token_id;
