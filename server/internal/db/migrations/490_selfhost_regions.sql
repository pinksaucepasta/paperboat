-- +goose Up
-- small-table: created empty here; at most 64 regions per scope.
SET LOCAL lock_timeout = '5s';
-- The owner's own regions: a display name ("Home lab") for the slug that nodes, pairs and pbh carry. The slug never
-- changes; only the name is renamed. A region that exists only as a node's region string (the default "selfhost", or one
-- typed at a pbh prompt) has no row until it is renamed. Names are unique per scope without regard to case; the check that
-- an explicit name does not collide with an implicit region (whose name is its slug) is made by the service under the
-- scope's advisory lock.
CREATE TABLE paperboat.selfhost_regions (
    scope_kind text NOT NULL CHECK (scope_kind IN ('account', 'team')),
    scope_id text NOT NULL,
    slug text NOT NULL CHECK (slug ~ '^[a-z][a-z0-9_-]{0,62}$'),
    display_name text NOT NULL CHECK (display_name = btrim(display_name) AND char_length(display_name) BETWEEN 1 AND 48),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (scope_kind, scope_id, slug)
);
CREATE UNIQUE INDEX selfhost_regions_display_name_key ON paperboat.selfhost_regions (scope_kind, scope_id, lower(display_name));

-- +goose Down
DROP TABLE paperboat.selfhost_regions;
