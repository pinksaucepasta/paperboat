-- +goose Up
-- The tunnel list starts from the hosts the account may manage tunnels on.
CREATE INDEX tunnels_created_by_host ON paperboat.tunnels(created_by_host_id, created_at DESC, id DESC);

-- +goose Down
DROP INDEX paperboat.tunnels_created_by_host;
