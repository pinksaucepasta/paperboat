-- +goose Up
SET LOCAL lock_timeout = '5s';
-- A peer policy document (peersessions/policy_cache.go) was dropped by every bare machine_events notification of its
-- account. Prod 11-19: one account's private_routes, previews and machine_services topics bump about once a second, so
-- every configuration renewal of its 168 endpoints was a full build (~300 ms, 19 slow builds a minute). Those topics
-- wake daemons (Watch reads the bare keys) but name nothing the document reads: the tables it reads each notify
-- 'policy:<key>' themselves (migrations 334-366, 418, 443; TestPeerPolicyInvalidationCoversEveryMutationPathOnPostgres).
-- The cache now honours only 'policy:' payloads, and the topic publishers send one for every topic but these three.
-- The invalidation suite showed two writes that only these topics named: a private tunnel route edit and the machine
-- service toggle. The tables a document reads through machine_service_authorities (tunnels, tunnel_routes and the
-- toggle's user_machines columns) now notify 'policy:' themselves; the edge-route, carrier and lease tables behind the
-- topics are not read by a document.

-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_peer_policy_tunnel() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_TABLE_NAME = 'tunnels' THEN
    PERFORM paperboat.publish_peer_policy(ARRAY['account:' || NEW.account_id, NEW.created_by_host_id]);
  ELSE
    PERFORM paperboat.publish_peer_policy(ARRAY(SELECT k FROM paperboat.tunnels t, LATERAL unnest(ARRAY['account:' || t.account_id, t.created_by_host_id]) k WHERE t.id = NEW.tunnel_id));
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_peer_policy_machine_service() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_peer_policy(ARRAY[NEW.id, 'account:' || NEW.user_id]);
  RETURN NULL;
END $$;
-- +goose StatementEnd

SELECT pg_advisory_xact_lock(1885686843);
CREATE TRIGGER peer_policy_tunnel_service AFTER UPDATE ON paperboat.tunnels FOR EACH ROW
 WHEN ((OLD.desired_state, OLD.access_mode, OLD.expires_at, OLD.deleted_at, OLD.created_by_host_id) IS DISTINCT FROM (NEW.desired_state, NEW.access_mode, NEW.expires_at, NEW.deleted_at, NEW.created_by_host_id))
 EXECUTE FUNCTION paperboat.publish_peer_policy_tunnel();
CREATE TRIGGER peer_policy_tunnel_route_service AFTER UPDATE ON paperboat.tunnel_routes FOR EACH ROW
 WHEN ((OLD.desired_state, OLD.deleted_at, OLD.generation, OLD.protocol, OLD.origin_address) IS DISTINCT FROM (NEW.desired_state, NEW.deleted_at, NEW.generation, NEW.protocol, NEW.origin_address))
 EXECUTE FUNCTION paperboat.publish_peer_policy_tunnel();
CREATE TRIGGER peer_policy_machine_service AFTER UPDATE ON paperboat.user_machines FOR EACH ROW
 WHEN ((OLD.personal_auto_enabled, OLD.machine_service_policy_generation) IS DISTINCT FROM (NEW.personal_auto_enabled, NEW.machine_service_policy_generation))
 EXECUTE FUNCTION paperboat.publish_peer_policy_machine_service();

-- +goose StatementBegin
CREATE FUNCTION paperboat.topic_invalidates_peer_policy(topic text) RETURNS boolean LANGUAGE sql IMMUTABLE AS $$
 SELECT topic NOT IN ('private_routes', 'previews', 'machine_services');
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_account_topic(account_ids text[], wanted_topic text) RETURNS void
    LANGUAGE sql AS $$
 WITH bumped AS (
  INSERT INTO paperboat.account_topic_bumps(account_id, topic)
  SELECT u.id, wanted_topic FROM paperboat.users u WHERE u.id = ANY (account_ids)
  RETURNING account_id)
 SELECT pg_notify('machine_events','account:'||account_id),
  CASE WHEN paperboat.topic_invalidates_peer_policy(wanted_topic) THEN pg_notify('machine_events','policy:account:'||account_id) END
 FROM bumped;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_machine_topic(machine_ids text[], wanted_topic text) RETURNS void
    LANGUAGE sql AS $$
 WITH bumped AS (
  INSERT INTO paperboat.machine_topic_generations(machine_id,topic,generation,updated_at)
  SELECT m.id,wanted_topic,1,now() FROM paperboat.user_machines m
  WHERE m.id = ANY (machine_ids)
  ON CONFLICT (machine_id,topic) DO UPDATE SET generation=paperboat.machine_topic_generations.generation+1,updated_at=now()
  RETURNING machine_id)
 SELECT pg_notify('machine_events',machine_id),
  CASE WHEN paperboat.topic_invalidates_peer_policy(wanted_topic) THEN pg_notify('machine_events','policy:'||machine_id) END
 FROM bumped;
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION '453 is forward-only: restore publish_account_topic from migration 450 and publish_machine_topic from migration 110'; END $$;
-- +goose StatementEnd
