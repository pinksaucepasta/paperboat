-- +goose Up
SET LOCAL lock_timeout = '5s';
-- small-table: an operator's handful of notices (price increase, terms change, subprocessor, breach), created empty here;
-- kept like billing records because they are the proof that a legally promised notice went out and when.
CREATE TABLE paperboat.customer_notices (
    id text PRIMARY KEY,
    seq bigint GENERATED ALWAYS AS IDENTITY,
    kind text NOT NULL CHECK (kind IN ('price_increase', 'terms_change', 'subprocessor', 'breach')),
    audience_kind text NOT NULL CHECK (audience_kind IN ('all', 'paid', 'plan', 'team', 'user')),
    audience_value text NOT NULL DEFAULT '' CHECK (length(audience_value) <= 128),
    subject text NOT NULL,
    html text NOT NULL,
    request_hash text NOT NULL,
    recipients integer NOT NULL DEFAULT 0,
    -- breach only: a notice sent more than 48 hours after the operator became aware (DPA, breach notice)
    late boolean NOT NULL DEFAULT false,
    created_by text NOT NULL,
    idempotency_key text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    queued_at timestamptz,
    UNIQUE (created_by, idempotency_key)
);
CREATE UNIQUE INDEX customer_notices_seq ON paperboat.customer_notices (seq DESC);
-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TABLE paperboat.customer_notices;
