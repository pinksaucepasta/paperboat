-- +goose Up
-- Runtime-issued bootstrap documents expire before cutover. In-progress
-- rotations must finish first: their signed proofs bind the previous hash.
-- +goose StatementBegin
DO $$
BEGIN
 IF EXISTS (SELECT 1 FROM paperboat.tunnel_connector_rotation_targets WHERE state NOT IN ('revoked','failed')) THEN
  RAISE EXCEPTION 'finish or cancel active connector credential rotations before endpoint identity cutover';
 END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE paperboat.tunnels DROP CONSTRAINT tunnels_stable_endpoint_id_check;
-- Goose executes this migration in one transaction. The named trigger DDL
-- takes an exclusive table lock; a failure rolls back the rekey and trigger state.
ALTER TABLE paperboat.tunnel_config_generations DISABLE TRIGGER tunnel_config_generations_immutable;
-- The URL is deliberately unchanged. Each existing endpoint keeps its UUIDv4
-- suffix; older non-v4 values receive a fresh current identity once.
-- +goose StatementBegin
DO $$
DECLARE tunnel record; cfg record; endpoint_id text; current_snapshot bytea;
BEGIN
 FOR tunnel IN SELECT id,stable_endpoint_id,generation FROM paperboat.tunnels FOR UPDATE LOOP
  endpoint_id := CASE WHEN tunnel.stable_endpoint_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'
   THEN 'endpoint_'||tunnel.stable_endpoint_id ELSE 'endpoint_'||gen_random_uuid()::text END;
  FOR cfg IN SELECT g.generation,g.snapshot,g.content_hash FROM paperboat.tunnel_config_generations g WHERE g.tunnel_id=tunnel.id FOR UPDATE LOOP
   IF sha256(cfg.snapshot)<>cfg.content_hash THEN
    RAISE EXCEPTION 'tunnel configuration integrity check failed before endpoint identity cutover';
   END IF;
   -- Owning writers persist compact, sorted JSON bytes. The validated URL
   -- scalar needs no escaping; insert its adjacent key without reserializing
   -- JSONB or changing any other content-addressed bytes.
   IF convert_from(cfg.snapshot,'UTF8') !~ '"stable_endpoint":"https://[a-z0-9.-]+"' THEN
    RAISE EXCEPTION 'tunnel configuration is not canonical managed endpoint JSON';
   END IF;
   current_snapshot := convert_to(regexp_replace(convert_from(cfg.snapshot,'UTF8'),
    '("stable_endpoint":"https://[a-z0-9.-]+")', '\1,"stable_endpoint_id":"'||endpoint_id||'"'),'UTF8');
   UPDATE paperboat.tunnel_config_generations SET snapshot=current_snapshot,content_hash=sha256(current_snapshot)
    WHERE tunnel_id=tunnel.id AND generation=cfg.generation;
  END LOOP;
  SELECT snapshot INTO current_snapshot FROM paperboat.tunnel_config_generations WHERE tunnel_id=tunnel.id AND generation=tunnel.generation;
  IF current_snapshot IS NOT NULL THEN
   current_snapshot := convert_to(replace(convert_from(current_snapshot,'UTF8'),'"generation":'||tunnel.generation||',','"generation":'||(tunnel.generation+1)||','),'UTF8');
   UPDATE paperboat.tunnel_config_generations SET activation_state='superseded',activated_at=NULL WHERE tunnel_id=tunnel.id AND activation_state='active';
   INSERT INTO paperboat.tunnel_config_generations(tunnel_id,generation,previous_generation,content_hash,snapshot,activation_state,created_by_actor_id,created_at,retained_until)
    SELECT tunnel.id,tunnel.generation+1,tunnel.generation,sha256(current_snapshot),current_snapshot,'pending',created_by_actor_id,now(),now()+interval '90 days'
    FROM paperboat.tunnel_config_generations WHERE tunnel_id=tunnel.id AND generation=tunnel.generation;
   UPDATE paperboat.tunnels SET generation=generation+1,updated_at=now() WHERE id=tunnel.id;
  END IF;
  UPDATE paperboat.tunnels SET stable_endpoint_id=endpoint_id WHERE id=tunnel.id;
 END LOOP;
END $$;
-- +goose StatementEnd
ALTER TABLE paperboat.tunnel_config_generations ENABLE TRIGGER tunnel_config_generations_immutable;
ALTER TABLE paperboat.tunnels ADD CONSTRAINT tunnels_stable_endpoint_id_check CHECK (stable_endpoint_id ~ '^endpoint_[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$');

-- Reject old admissions before freshly signed descriptors and snapshots issue.
UPDATE paperboat.tunnel_connector_sessions SET state='disconnected',disconnected_at=now(),disconnect_reason_code='configuration_changed',ready_at=NULL WHERE state NOT IN ('disconnected','expired');
UPDATE paperboat.tunnel_connectors SET last_applied_config_generation=0,ready_at=NULL,last_heartbeat_at=NULL,updated_at=now();
UPDATE paperboat.tunnel_edge_route_assignments SET state='detached',observed_state='detached',released_at=now(),updated_at=now() WHERE state<>'detached';
