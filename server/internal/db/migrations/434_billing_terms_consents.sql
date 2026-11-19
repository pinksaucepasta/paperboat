-- +goose Up
SET LOCAL lock_timeout = '5s';
-- small-table: one row per paid checkout, top-up or paid team creation; kept like billing records (legal evidence
-- of the no-refund consent), not trimmed by retention; account deletion leaves the pseudonymous user id.
CREATE TABLE paperboat.billing_terms_consents (
    operation_id text PRIMARY KEY CHECK (length(operation_id) BETWEEN 1 AND 200),
    actor_user_id text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('checkout', 'topup', 'team_create')),
    terms_version text NOT NULL CHECK (length(terms_version) BETWEEN 1 AND 64),
    text_sha256 text NOT NULL CHECK (length(text_sha256) = 64),
    accepted_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX billing_terms_consents_actor ON paperboat.billing_terms_consents (actor_user_id, accepted_at DESC);
-- +goose Down
DROP TABLE paperboat.billing_terms_consents;
