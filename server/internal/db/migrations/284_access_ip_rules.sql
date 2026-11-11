-- +goose Up
SET LOCAL lock_timeout = '5s';
-- Owner IP allow and deny lists per tunnel or preview (edge abuse plan 3.5): CIDRs, at most 256 each.
-- An empty pair is deleted, not stored, so the table holds only resources with rules.
CREATE TABLE paperboat.access_ip_rules (
    resource_kind text NOT NULL CHECK (resource_kind IN ('tunnel', 'preview')),
    resource_id text NOT NULL CHECK (char_length(resource_id) BETWEEN 1 AND 128),
    allow_cidrs text[] NOT NULL DEFAULT '{}' CHECK (cardinality(allow_cidrs) <= 256),
    deny_cidrs text[] NOT NULL DEFAULT '{}' CHECK (cardinality(deny_cidrs) <= 256),
    updated_by text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (resource_kind, resource_id)
);

-- +goose Down
DROP TABLE paperboat.access_ip_rules;
