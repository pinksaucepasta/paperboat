-- +goose Up
ALTER TABLE paperboat.browser_access_transactions
  ADD COLUMN route_id text,
  ADD COLUMN viewer_policy_generation bigint;

ALTER TABLE paperboat.browser_access_sessions
  ADD COLUMN route_id text,
  ADD COLUMN viewer_policy_generation bigint;

ALTER TABLE paperboat.browser_access_transactions
  ADD CONSTRAINT browser_access_transactions_viewer_policy_generation_check
    CHECK (viewer_policy_generation IS NULL OR viewer_policy_generation >= 0);
ALTER TABLE paperboat.browser_access_sessions
  ADD CONSTRAINT browser_access_sessions_viewer_policy_generation_check
    CHECK (viewer_policy_generation IS NULL OR viewer_policy_generation >= 0);

CREATE TABLE paperboat.browser_viewer_policies (
  resource_kind text NOT NULL,
  resource_id text NOT NULL,
  route_id text NOT NULL REFERENCES paperboat.tunnel_routes(id) ON DELETE CASCADE,
  owner_account_id text NOT NULL REFERENCES paperboat.users(id) ON DELETE CASCADE,
  audience text NOT NULL,
  team_id text REFERENCES paperboat.teams(team_id),
  allowed_emails text[] NOT NULL DEFAULT '{}',
  email_domain text,
  generation bigint NOT NULL DEFAULT 1,
  updated_by_account text NOT NULL REFERENCES paperboat.users(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (resource_kind, resource_id, route_id),
  CONSTRAINT browser_viewer_policies_resource_kind_check
    CHECK (resource_kind IN ('preview', 'tunnel')),
  CONSTRAINT browser_viewer_policies_audience_check
    CHECK (audience IN ('owner', 'team', 'specific_emails', 'email_domain', 'public')),
  CONSTRAINT browser_viewer_policies_generation_check CHECK (generation > 0),
  CONSTRAINT browser_viewer_policies_selectors_check CHECK (
    (audience IN ('owner', 'public') AND team_id IS NULL AND cardinality(allowed_emails) = 0 AND email_domain IS NULL)
    OR (audience = 'team' AND team_id IS NOT NULL AND cardinality(allowed_emails) = 0 AND email_domain IS NULL)
    OR (audience = 'specific_emails' AND team_id IS NULL AND cardinality(allowed_emails) > 0 AND email_domain IS NULL)
    OR (audience = 'email_domain' AND team_id IS NULL AND cardinality(allowed_emails) = 0 AND email_domain IS NOT NULL)
  ),
  CONSTRAINT browser_viewer_policies_email_domain_check
    CHECK (email_domain IS NULL OR (email_domain = lower(email_domain) AND email_domain !~ '[^a-z0-9.-]' AND email_domain !~ '(^[.-]|[.-]$|\.\.)')),
  CONSTRAINT browser_viewer_policies_allowed_emails_check
    CHECK (cardinality(allowed_emails) <= 100)
);

CREATE INDEX browser_viewer_policies_route ON paperboat.browser_viewer_policies(route_id);

-- +goose Down
DROP TABLE paperboat.browser_viewer_policies;
ALTER TABLE paperboat.browser_access_sessions
  DROP CONSTRAINT browser_access_sessions_viewer_policy_generation_check,
  DROP COLUMN viewer_policy_generation,
  DROP COLUMN route_id;
ALTER TABLE paperboat.browser_access_transactions
  DROP CONSTRAINT browser_access_transactions_viewer_policy_generation_check,
  DROP COLUMN viewer_policy_generation,
  DROP COLUMN route_id;
