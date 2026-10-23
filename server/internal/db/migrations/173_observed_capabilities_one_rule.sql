-- +goose Up
-- observed_capabilities had two writers with different sets: the runtime observation (every heartbeat:
-- file_receive, preview_launch, terminal_host, session_host, keep_awake[, environment_injection]) and the
-- capability-apply report (file_receive, preview_launch, terminal_host, session_host, ssh_host). The
-- column flipped on every write, so 162's inventory trigger stormed and had to be reverted (171).
-- Both writers now compute the column with one function: the runtime's reported set, plus ssh_host when
-- the applied capability selection has it (only the apply report knows ssh_host), sorted and deduplicated. Equal inputs give an equal value, so the trigger moves the inventory topic
-- only on a real change (start-up, update, capability change).
-- Prior art: Tailscale keeps one Hostinfo per node, sent by the node, and sends a peer change only
-- when it differs.
-- The capability-apply report's selection gets its own column: it used to be read back from
-- observed_capabilities, which the runtime also wrote.
ALTER TABLE paperboat.user_machines ADD COLUMN capabilities_applied text[] NOT NULL DEFAULT '{}';
UPDATE paperboat.user_machines SET capabilities_applied = CASE
  WHEN capabilities_status = 'applied' AND capabilities_observed_version = capabilities_desired_version THEN configured_capabilities
  ELSE coalesce(observed_capabilities, '{}') END;
-- +goose StatementBegin
CREATE FUNCTION paperboat.machine_observed_capabilities(reported text[], applied text[])
RETURNS text[] LANGUAGE sql IMMUTABLE AS $$
  SELECT coalesce(array_agg(DISTINCT c ORDER BY c), '{}'::text[])
  FROM unnest(array_remove(coalesce(reported, '{}'::text[]), 'ssh_host')
    || CASE WHEN 'ssh_host' = ANY(applied) THEN ARRAY['ssh_host'] ELSE '{}'::text[] END) AS c
$$;
-- +goose StatementEnd
DROP TRIGGER user_machines_publish_inventory_update ON paperboat.user_machines;
CREATE TRIGGER user_machines_publish_inventory_update
AFTER UPDATE OF alias, state, seat_state, deleted_at, revoked_at, owner_team_id, last_seen_at, observed_capabilities ON paperboat.user_machines
FOR EACH ROW WHEN (
  old.alias IS DISTINCT FROM new.alias
  OR (old.state IS DISTINCT FROM new.state AND NOT (old.state IN ('online','offline') AND new.state IN ('online','offline')))
  OR (old.last_seen_at IS NULL AND new.last_seen_at IS NOT NULL)
  OR old.seat_state IS DISTINCT FROM new.seat_state
  OR old.deleted_at IS DISTINCT FROM new.deleted_at
  OR old.revoked_at IS DISTINCT FROM new.revoked_at
  OR old.owner_team_id IS DISTINCT FROM new.owner_team_id
  OR NOT (coalesce(old.observed_capabilities, '{}') @> coalesce(new.observed_capabilities, '{}')
      AND coalesce(new.observed_capabilities, '{}') @> coalesce(old.observed_capabilities, '{}')))
EXECUTE FUNCTION paperboat.publish_machine_inventory();
-- Existing rows take the one rule's value now, so the first heartbeat after the deploy changes nothing.
UPDATE paperboat.user_machines SET observed_capabilities = paperboat.machine_observed_capabilities(observed_capabilities, capabilities_applied)
WHERE observed_capabilities IS DISTINCT FROM paperboat.machine_observed_capabilities(observed_capabilities, capabilities_applied);

-- +goose Down
DROP TRIGGER user_machines_publish_inventory_update ON paperboat.user_machines;
CREATE TRIGGER user_machines_publish_inventory_update
AFTER UPDATE OF alias, state, seat_state, deleted_at, revoked_at, owner_team_id, last_seen_at ON paperboat.user_machines
FOR EACH ROW WHEN (
  old.alias IS DISTINCT FROM new.alias
  OR (old.state IS DISTINCT FROM new.state AND NOT (old.state IN ('online','offline') AND new.state IN ('online','offline')))
  OR (old.last_seen_at IS NULL AND new.last_seen_at IS NOT NULL)
  OR old.seat_state IS DISTINCT FROM new.seat_state
  OR old.deleted_at IS DISTINCT FROM new.deleted_at
  OR old.revoked_at IS DISTINCT FROM new.revoked_at
  OR old.owner_team_id IS DISTINCT FROM new.owner_team_id)
EXECUTE FUNCTION paperboat.publish_machine_inventory();
DROP FUNCTION paperboat.machine_observed_capabilities(text[], text[]);
ALTER TABLE paperboat.user_machines DROP COLUMN capabilities_applied;
