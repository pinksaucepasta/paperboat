-- +goose Up
-- One floor row per node component (none set: an empty minimum version).
INSERT INTO paperboat.client_update_policy(component, singleton) VALUES ('pbh', false), ('edge', false), ('relay', false)
ON CONFLICT (component) DO NOTHING;

-- +goose Down
DELETE FROM paperboat.client_update_policy WHERE component <> 'pb';
