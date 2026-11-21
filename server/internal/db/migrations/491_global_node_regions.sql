-- +goose Up
-- Paperboat's own nodes are pbh installations claimed with global scope (one install path, like self-hosters); their
-- region is the one they were installed with. Give every such region an operator region record before 492 makes those
-- nodes regional, so none drops out of service for want of an enabled region. Bounded: one row per distinct region.
INSERT INTO paperboat.operator_regions(id,display_name)
SELECT DISTINCT n.region,n.region FROM paperboat.selfhost_installations i JOIN paperboat.control_tunnel_nodes n ON n.id=i.node_id
WHERE i.scope_kind='global' AND i.state='active' AND n.region ~ '^[a-z][a-z0-9_-]{0,62}$'
ON CONFLICT (id) DO NOTHING;

-- +goose Down
SELECT 1;
