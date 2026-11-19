-- +goose Up
-- Aggregate traffic for the operator dashboard: bytes per hour, category and node, no connection, address or content.
-- Two writers: a trigger on resource_bandwidth_events (direct, machine to machine, as the hosts report it) and the node
-- heartbeat (bytes a relay or edge says it carried). The retention worker folds hours past their window into days.
-- shard spreads the direct rows of one hour over 16 rows so concurrent machine reports do not queue on one row lock.
SET lock_timeout = '5s';

CREATE TABLE paperboat.traffic_hourly (
    hour timestamptz NOT NULL,
    category text NOT NULL CHECK (category IN ('direct','relay_managed','relay_self_hosted','edge_managed','edge_self_hosted')),
    node_id text NOT NULL DEFAULT '',
    shard smallint NOT NULL DEFAULT 0,
    upload_bytes bigint NOT NULL DEFAULT 0 CHECK (upload_bytes >= 0),
    download_bytes bigint NOT NULL DEFAULT 0 CHECK (download_bytes >= 0),
    PRIMARY KEY (hour, category, node_id, shard)
);

CREATE TABLE paperboat.traffic_daily (
    day date NOT NULL,
    category text NOT NULL CHECK (category IN ('direct','relay_managed','relay_self_hosted','edge_managed','edge_self_hosted')),
    node_id text NOT NULL DEFAULT '',
    upload_bytes bigint NOT NULL DEFAULT 0 CHECK (upload_bytes >= 0),
    download_bytes bigint NOT NULL DEFAULT 0 CHECK (download_bytes >= 0),
    PRIMARY KEY (day, category, node_id)
);

-- +goose StatementBegin
CREATE FUNCTION paperboat.add_direct_traffic() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE up bigint; down bigint;
BEGIN
 -- Native reports are cumulative per stream and minute: an update adds only the growth.
 IF TG_OP = 'INSERT' THEN up := NEW.upload_bytes; down := NEW.download_bytes;
 ELSE up := NEW.upload_bytes-OLD.upload_bytes; down := NEW.download_bytes-OLD.download_bytes; END IF;
 IF up > 0 OR down > 0 OR TG_OP = 'INSERT' THEN
  INSERT INTO paperboat.traffic_hourly(hour,category,node_id,shard,upload_bytes,download_bytes)
  VALUES(date_trunc('hour',NEW.interval_start AT TIME ZONE 'UTC') AT TIME ZONE 'UTC','direct','',abs(hashtext(NEW.operation_id))%16,up,down)
  ON CONFLICT(hour,category,node_id,shard) DO UPDATE SET upload_bytes=traffic_hourly.upload_bytes+EXCLUDED.upload_bytes,download_bytes=traffic_hourly.download_bytes+EXCLUDED.download_bytes;
 END IF;
 RETURN NULL;
END $$;
-- +goose StatementEnd

CREATE TRIGGER resource_bandwidth_direct_traffic_insert AFTER INSERT ON paperboat.resource_bandwidth_events
 FOR EACH ROW WHEN (NEW.mode = 'direct') EXECUTE FUNCTION paperboat.add_direct_traffic();
CREATE TRIGGER resource_bandwidth_direct_traffic_update AFTER UPDATE OF upload_bytes, download_bytes ON paperboat.resource_bandwidth_events
 FOR EACH ROW WHEN (NEW.mode = 'direct') EXECUTE FUNCTION paperboat.add_direct_traffic();

-- +goose Down
DROP TRIGGER resource_bandwidth_direct_traffic_update ON paperboat.resource_bandwidth_events;
DROP TRIGGER resource_bandwidth_direct_traffic_insert ON paperboat.resource_bandwidth_events;
DROP FUNCTION paperboat.add_direct_traffic();
DROP TABLE paperboat.traffic_daily;
DROP TABLE paperboat.traffic_hourly;
