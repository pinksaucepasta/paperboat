-- +goose Up
SET LOCAL lock_timeout = '5s';
-- Recipes (PLAN F26): a recipe pairs and sets up machines with one token. The token is
-- pbr_<token_id>_<secret>; the machine sends auth = HKDF(secret, "paperboat recipe auth") and only
-- sha256(auth) is stored. A recipe acts as its creator (account_id) in one workspace (team_id NULL
-- is the personal workspace). source keeps the file as written (comments kept); canonical is the
-- validated JSON (shared/recipe). expires_at ends the token, not the machines.
CREATE TABLE paperboat.recipes (
    id text PRIMARY KEY,
    token_id text NOT NULL UNIQUE,
    account_id text NOT NULL REFERENCES paperboat.users(id),
    team_id text,
    name text NOT NULL,
    revision integer NOT NULL DEFAULT 1 CHECK (revision >= 1),
    source text NOT NULL,
    source_format text NOT NULL CHECK (source_format IN ('toml', 'json')),
    canonical jsonb NOT NULL CHECK (jsonb_typeof(canonical) = 'object'),
    auth_hash bytea NOT NULL UNIQUE CHECK (octet_length(auth_hash) = 32),
    previous_auth_hash bytea CHECK (octet_length(previous_auth_hash) = 32),
    rotated_at timestamptz,
    max_machines integer CHECK (max_machines >= 1),
    remove_after_offline interval,
    machine_lifetime interval,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    last_used_at timestamptz,
    revoked_at timestamptz,
    revoked_by text,
    CONSTRAINT recipes_expiry CHECK (expires_at > created_at),
    CONSTRAINT recipes_revocation CHECK ((revoked_at IS NULL) = (revoked_by IS NULL))
);
CREATE UNIQUE INDEX recipes_live_name ON paperboat.recipes (coalesce(team_id, 'personal:' || account_id), name) WHERE revoked_at IS NULL;
CREATE INDEX recipes_workspace ON paperboat.recipes (coalesce(team_id, ''), created_at DESC);

-- What a recipe machine's pb may reach, stored like policy rules: machines by ID, tags and every
-- machine by name, resolved per request against the workspace policy tags. No rows: nothing.
CREATE TABLE paperboat.recipe_reach_rules (
    recipe_id text NOT NULL REFERENCES paperboat.recipes(id) ON DELETE CASCADE,
    entry integer NOT NULL CHECK (entry >= 0),
    target_kind text NOT NULL CHECK (target_kind IN ('all_machines', 'machine', 'tag', 'preview', 'tunnel', 'env')),
    target_id text NOT NULL,
    action text NOT NULL,
    ports int4range NOT NULL
);
CREATE INDEX recipe_reach_rules_lookup ON paperboat.recipe_reach_rules (recipe_id, action);

-- Machines paired from a recipe. remove_at is the machine_lifetime deadline; offline_remove_at is
-- set when the machine's last control connection ends (remove_after_offline) and cleared when it
-- connects again. The collector revokes a machine once either deadline passes.
CREATE TABLE paperboat.recipe_machines (
    machine_id text PRIMARY KEY REFERENCES paperboat.user_machines(id) ON DELETE CASCADE,
    recipe_id text NOT NULL REFERENCES paperboat.recipes(id) ON DELETE CASCADE,
    applied_revision integer NOT NULL DEFAULT 0,
    apply_state jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(apply_state) = 'object'),
    paired_at timestamptz NOT NULL DEFAULT now(),
    remove_at timestamptz,
    offline_remove_at timestamptz
);
CREATE INDEX recipe_machines_recipe ON paperboat.recipe_machines (recipe_id);
CREATE INDEX recipe_machines_due ON paperboat.recipe_machines (least(remove_at, offline_remove_at)) WHERE remove_at IS NOT NULL OR offline_remove_at IS NOT NULL;

-- Whether a recipe's reach covers one machine action (port NULL: any port). The caller allows the
-- session's own machine without asking.
-- +goose StatementBegin
CREATE FUNCTION paperboat.recipe_reach_allows(recipe text, machine text, capability text, port integer) RETURNS boolean
    LANGUAGE sql STABLE
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
 SELECT EXISTS (
  SELECT 1 FROM recipes t
  JOIN recipe_reach_rules r ON r.recipe_id = t.id
  LEFT JOIN workspace_policies p ON (t.team_id IS NOT NULL AND p.team_id = t.team_id) OR (t.team_id IS NULL AND p.owner_account = t.account_id)
  WHERE t.id = recipe AND t.revoked_at IS NULL AND r.action = capability AND (port IS NULL OR r.ports @> port)
   AND (r.target_kind = 'all_machines'
     OR (r.target_kind = 'machine' AND r.target_id = machine)
     OR (r.target_kind = 'tag' AND EXISTS(SELECT 1 FROM workspace_policy_tags tg WHERE tg.policy_id = p.id AND tg.tag = r.target_id AND tg.machine_id = machine))));
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION paperboat.recipe_reach_allows(text, text, text, integer);
DROP TABLE paperboat.recipe_machines;
DROP TABLE paperboat.recipe_reach_rules;
DROP TABLE paperboat.recipes;
