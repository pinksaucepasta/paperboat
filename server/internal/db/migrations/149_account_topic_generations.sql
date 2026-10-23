-- +goose Up
-- Audience-wide machine topics (inventory, peer_network, previews, machine_services) bumped one generation row per
-- machine of the audience, and an audience is every machine of one or more accounts: one change on an account with
-- N machines wrote N rows (pairing on a 10k-machine account, 10k writes; a 4,097-row fixture insert ran 5.5 h). They
-- now bump one row per account. A machine waiting on a topic sees its own row plus its account's row (both only
-- grow, so their sum only grows and daemons' known generations stay valid); targeted topics (machine_access,
-- tunnels, config) keep their machine row.
CREATE TABLE paperboat.account_topic_generations (
    account_id text NOT NULL REFERENCES paperboat.users(id) ON DELETE CASCADE,
    topic text NOT NULL CHECK (topic IN ('machine_access', 'peer_network', 'previews', 'tunnels', 'config', 'inventory', 'machine_services')),
    generation bigint NOT NULL CHECK (generation > 0),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, topic)
);

-- The accounts whose machines are the audience of one machine: its owner, the members of its owning team, and the
-- members of teams it is shared into who may use it (the same rule as machine_audience).
-- +goose StatementBegin
CREATE FUNCTION paperboat.machine_audience_accounts(wanted_machine text) RETURNS text[]
    LANGUAGE sql STABLE AS $$
 SELECT coalesce(array_agg(DISTINCT audience.account_id), '{}'::text[]) FROM (
  SELECT own.user_id AS account_id FROM paperboat.user_machines own WHERE own.id = wanted_machine
  UNION
  SELECT member.account_id FROM paperboat.user_machines own
   JOIN paperboat.team_members member ON member.team_id = own.owner_team_id AND member.active
   WHERE own.id = wanted_machine AND own.owner_team_id IS NOT NULL
  UNION
  SELECT member.account_id FROM paperboat.team_resource_bindings b
   JOIN paperboat.team_members member ON member.team_id = b.team_id AND member.active
   WHERE b.resource_kind = 'machine' AND b.resource_id = wanted_machine AND b.active
    AND EXISTS(SELECT 1 FROM unnest(ARRAY['terminal','exec','managed_ssh','files','preview_manage','tunnel_manage','private_access']) cap
      WHERE NOT isempty(paperboat.workspace_policy_ports(member.account_id, b.team_id, wanted_machine, cap)))
 ) audience
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_account_topic(account_ids text[], wanted_topic text) RETURNS void
    LANGUAGE sql AS $$
 WITH bumped AS (
  INSERT INTO paperboat.account_topic_generations(account_id,topic,generation,updated_at)
  SELECT u.id,wanted_topic,1,now() FROM paperboat.users u WHERE u.id = ANY (account_ids)
  ON CONFLICT (account_id,topic) DO UPDATE SET generation=paperboat.account_topic_generations.generation+1,updated_at=now()
  RETURNING account_id)
 SELECT pg_notify('machine_events','account:'||account_id) FROM bumped;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_machine_access_grant() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_machine_topic(ARRAY[NEW.machine_id], 'machine_access');
  PERFORM paperboat.publish_account_topic(paperboat.machine_audience_accounts(NEW.machine_id), 'inventory');
  RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_machine_inventory() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_account_topic(paperboat.machine_audience_accounts(NEW.id), 'inventory');
  RETURN NEW;
END $$;
-- +goose StatementEnd

-- A machine has no team resource bindings yet when it is inserted, so its audience is its account and, for a
-- team-owned machine, the team's members (as 141).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_machine_inventory_inserted() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_account_topic(ARRAY(
    SELECT DISTINCT user_id FROM inserted
    UNION
    SELECT member.account_id FROM inserted i
     JOIN paperboat.team_members member ON member.team_id = i.owner_team_id AND member.active
     WHERE i.owner_team_id IS NOT NULL), 'inventory');
  RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_machine_preview() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_account_topic(paperboat.machine_audience_accounts(NEW.owner_machine_id), 'previews');
  RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_machine_services() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_account_topic(paperboat.machine_audience_accounts(NEW.machine_id), 'machine_services');
  RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_team_membership() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_account_topic(ARRAY[NEW.account_id], 'inventory');
  RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_peer_identity() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.machine_id IS NOT NULL THEN
    PERFORM paperboat.publish_account_topic(paperboat.machine_audience_accounts(NEW.machine_id), 'peer_network');
  ELSE
    PERFORM paperboat.publish_account_topic(ARRAY[NEW.user_id], 'peer_network');
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_peer_certificate_revocation() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_account_topic(ARRAY[NEW.user_id], 'peer_network');
  RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_machine_access_grant() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_machine_topic(ARRAY[NEW.machine_id], 'machine_access');
  PERFORM paperboat.publish_machine_topic(paperboat.machine_audience(NEW.machine_id), 'inventory');
  RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_machine_inventory() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_machine_topic(paperboat.machine_audience(NEW.id), 'inventory');
  RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_machine_inventory_inserted() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_machine_topic(ARRAY(
    SELECT peer.id FROM paperboat.user_machines peer
     WHERE peer.deleted_at IS NULL AND peer.user_id IN (SELECT DISTINCT user_id FROM inserted)
    UNION
    SELECT peer.id FROM inserted i
     JOIN paperboat.team_members member ON member.team_id = i.owner_team_id AND member.active
     JOIN paperboat.user_machines peer ON peer.user_id = member.account_id AND peer.deleted_at IS NULL
     WHERE i.owner_team_id IS NOT NULL), 'inventory');
  RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_machine_preview() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_machine_topic(paperboat.machine_audience(NEW.owner_machine_id), 'previews');
  RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_machine_services() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_machine_topic(paperboat.machine_audience(NEW.machine_id), 'machine_services');
  RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_team_membership() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_machine_topic(paperboat.account_machine_ids(NEW.account_id), 'inventory');
  RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_peer_identity() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.machine_id IS NOT NULL THEN
    PERFORM paperboat.publish_machine_topic(paperboat.machine_audience(NEW.machine_id), 'peer_network');
  ELSE
    PERFORM paperboat.publish_machine_topic(paperboat.account_machine_ids(NEW.user_id), 'peer_network');
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_peer_certificate_revocation() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_machine_topic(paperboat.account_machine_ids(NEW.user_id), 'peer_network');
  RETURN NEW;
END $$;
-- +goose StatementEnd

DROP FUNCTION paperboat.publish_account_topic(text[], text);
DROP FUNCTION paperboat.machine_audience_accounts(text);
DROP TABLE paperboat.account_topic_generations;
