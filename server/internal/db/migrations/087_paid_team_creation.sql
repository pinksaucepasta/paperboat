-- +goose Up
CREATE TABLE paperboat.billing_team_orders (
 operation_id text PRIMARY KEY,
 team_id text NOT NULL,
 actor_user_id text NOT NULL REFERENCES paperboat.users(id),
 request_hash text NOT NULL,
 request_ciphertext bytea NOT NULL,
 result_ciphertext bytea,
 provider_customer_id text UNIQUE,
 provider_session_id text UNIQUE,
 state text NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','checkout','paid','failed')),
 attempted_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX billing_team_orders_reserved_team ON paperboat.billing_team_orders(team_id) WHERE state<>'failed';
-- +goose Down
DROP TABLE paperboat.billing_team_orders;
