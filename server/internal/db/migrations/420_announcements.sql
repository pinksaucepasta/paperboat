-- +goose Up
SET LOCAL lock_timeout = '5s';
-- small-table: created empty here; announcements are an operator's handful, kept 90 days after they end.
-- Operator announcements: a notice (a row in the console bell) or a banner (the ribbon above every page), for everyone,
-- a plan, a team or one user. Rows past their end or retracted are deleted by retention; receipts go with them.
CREATE TABLE paperboat.announcements (
    id text PRIMARY KEY,
    seq bigint GENERATED ALWAYS AS IDENTITY,
    kind text NOT NULL CHECK (kind IN ('notice', 'banner')),
    title text NOT NULL CHECK (length(title) BETWEEN 1 AND 80),
    body text NOT NULL DEFAULT '' CHECK (length(body) <= 280),
    link_url text CHECK (link_url IS NULL OR (length(link_url) <= 512 AND link_url ~ '^(https://[^[:space:]]+|/([^/[:space:]\\][^[:space:]\\]*)?)$')),
    severity text NOT NULL DEFAULT 'info' CHECK (severity IN ('info', 'warning', 'critical')),
    dismissible boolean NOT NULL DEFAULT true,
    audience_kind text NOT NULL CHECK (audience_kind IN ('all', 'plan', 'team', 'user')),
    audience_value text NOT NULL DEFAULT '' CHECK (length(audience_value) <= 128 AND ((audience_kind = 'all') = (audience_value = ''))),
    starts_at timestamptz NOT NULL,
    ends_at timestamptz,
    created_by text NOT NULL,
    idempotency_key text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    retracted_at timestamptz,
    retracted_by text,
    CHECK (ends_at IS NULL OR ends_at > starts_at),
    CHECK ((retracted_at IS NULL) = (retracted_by IS NULL)),
    UNIQUE (created_by, idempotency_key)
);
CREATE UNIQUE INDEX announcements_seq ON paperboat.announcements (seq DESC);
CREATE INDEX announcements_live ON paperboat.announcements (kind, starts_at DESC) WHERE retracted_at IS NULL;

-- What a user did with an announcement: read a notice, or dismissed a banner.
CREATE TABLE paperboat.announcement_receipts (
    announcement_id text NOT NULL REFERENCES paperboat.announcements (id) ON DELETE CASCADE,
    user_id text NOT NULL REFERENCES paperboat.users (id) ON DELETE CASCADE,
    read_at timestamptz,
    dismissed_at timestamptz,
    PRIMARY KEY (announcement_id, user_id)
);

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TABLE paperboat.announcement_receipts;
DROP TABLE paperboat.announcements;
