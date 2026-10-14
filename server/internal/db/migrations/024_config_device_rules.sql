-- +goose Up
ALTER TABLE paperboat.control_config_assignments
 ADD COLUMN path_rules jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(path_rules) = 'array' AND jsonb_array_length(path_rules) <= 256),
 ADD COLUMN repository_bindings jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(repository_bindings) = 'array' AND jsonb_array_length(repository_bindings) <= 2);

-- +goose Down
ALTER TABLE paperboat.control_config_assignments DROP COLUMN repository_bindings, DROP COLUMN path_rules;
