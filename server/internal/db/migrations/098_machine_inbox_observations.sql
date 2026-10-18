-- +goose Up
-- What each machine reports about its Paperboat Inbox: names, sizes and times only, never contents.
CREATE TABLE paperboat.machine_inbox_observations (
 user_machine_id text PRIMARY KEY REFERENCES paperboat.user_machines(id) ON DELETE CASCADE,
 environment_id text NOT NULL,
 items jsonb NOT NULL DEFAULT '[]'::jsonb,
 total integer NOT NULL CHECK (total >= 0),
 observed_at timestamptz NOT NULL,
 updated_at timestamptz NOT NULL DEFAULT now(),
 CHECK (jsonb_typeof(items) = 'array' AND jsonb_array_length(items) <= 200)
);

-- +goose Down
DROP TABLE paperboat.machine_inbox_observations;
