-- +goose Up
CREATE TABLE paperboat.client_update_policy (
 singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
 revision bigint NOT NULL DEFAULT 0 CHECK (revision >= 0),
 minimum_version text NOT NULL DEFAULT '' CHECK (length(minimum_version) <= 64),
 reason text NOT NULL DEFAULT '' CHECK (octet_length(reason) <= 256),
 enforce_at timestamptz
);
INSERT INTO paperboat.client_update_policy(singleton) VALUES(true);
CREATE TABLE paperboat.client_update_policy_requests (
 actor_id text NOT NULL,
 key_digest text NOT NULL,
 request_digest text NOT NULL,
 response jsonb NOT NULL,
 expires_at timestamptz NOT NULL,
 PRIMARY KEY(actor_id,key_digest)
);
CREATE INDEX client_update_policy_requests_expiry ON paperboat.client_update_policy_requests(expires_at);
-- +goose Down
DROP TABLE paperboat.client_update_policy_requests;
DROP TABLE paperboat.client_update_policy;
