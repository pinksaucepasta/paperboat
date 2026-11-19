-- +goose Up
-- small-table: one row per component. The update floor covers pbh, edge and relay as well as pb: each component
-- gets its own minimum version. singleton stays (the previous server reads WHERE singleton = true, which keeps
-- matching only pb's row); a post-deploy migration drops it once no running server reads it.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.client_update_policy ADD COLUMN component text NOT NULL DEFAULT 'pb' CHECK (component IN ('pb', 'pbh', 'edge', 'relay'));
ALTER TABLE paperboat.client_update_policy DROP CONSTRAINT client_update_policy_singleton_check;
ALTER TABLE paperboat.client_update_policy DROP CONSTRAINT client_update_policy_pkey;
ALTER TABLE paperboat.client_update_policy ADD PRIMARY KEY (component);
ALTER TABLE paperboat.client_update_policy ADD CONSTRAINT client_update_policy_singleton_is_pb CHECK (singleton = (component = 'pb')) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.client_update_policy DROP CONSTRAINT client_update_policy_singleton_is_pb;
ALTER TABLE paperboat.client_update_policy DROP CONSTRAINT client_update_policy_pkey;
ALTER TABLE paperboat.client_update_policy ADD PRIMARY KEY (singleton);
ALTER TABLE paperboat.client_update_policy ADD CONSTRAINT client_update_policy_singleton_check CHECK (singleton) NOT VALID;
ALTER TABLE paperboat.client_update_policy DROP COLUMN component;
