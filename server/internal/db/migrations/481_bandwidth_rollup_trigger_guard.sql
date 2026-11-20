-- +goose Up
SET LOCAL lock_timeout = '5s';
-- Old usage history is merged into one row per hour and resource by the retention worker (rollup:* operation ids). Those rows
-- carry bytes the dashboard rollup already counted when the raw rows were written, so they must not be counted again.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.add_direct_traffic() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE up bigint; down bigint;
BEGIN
 IF NEW.operation_id LIKE 'rollup:%' THEN RETURN NULL; END IF;
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

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.add_direct_traffic() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE up bigint; down bigint;
BEGIN
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
