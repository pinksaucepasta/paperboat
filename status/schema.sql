-- D1 schema. Free tier: 100k rows written a day, so a probe writes one row a minute (all its results in one JSON value).
-- reports: one row per vantage point and minute bucket; ts is the unrounded receipt time (freshness), probe_ts the signed time (replay guard).
CREATE TABLE IF NOT EXISTS reports (
  vantage TEXT NOT NULL, bucket INTEGER NOT NULL, ts INTEGER NOT NULL, probe_ts INTEGER NOT NULL DEFAULT 0, net_ok INTEGER NOT NULL,
  build TEXT NOT NULL DEFAULT '', results TEXT NOT NULL DEFAULT '{}', PRIMARY KEY (vantage, bucket));
CREATE TABLE IF NOT EXISTS unit_state (unit TEXT PRIMARY KEY, state TEXT NOT NULL, since INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS kv (k TEXT PRIMARY KEY, v TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS uptime_day (
  component TEXT NOT NULL, day TEXT NOT NULL, up INTEGER NOT NULL DEFAULT 0, degraded INTEGER NOT NULL DEFAULT 0,
  down INTEGER NOT NULL DEFAULT 0, delayed INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (component, day));
CREATE TABLE IF NOT EXISTS incidents (
  id TEXT PRIMARY KEY, title TEXT NOT NULL, status TEXT NOT NULL, components TEXT NOT NULL,
  created INTEGER NOT NULL, updated INTEGER NOT NULL, resolved INTEGER);
CREATE TABLE IF NOT EXISTS incident_updates (
  id TEXT PRIMARY KEY, incident_id TEXT NOT NULL, status TEXT NOT NULL, body TEXT NOT NULL, ts INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS incident_updates_incident ON incident_updates (incident_id, ts);
CREATE TABLE IF NOT EXISTS maintenance (
  id TEXT PRIMARY KEY, title TEXT NOT NULL, body TEXT NOT NULL DEFAULT '', components TEXT NOT NULL,
  starts INTEGER NOT NULL, ends INTEGER NOT NULL, created INTEGER NOT NULL);
-- snapshot: at is the last evaluation time; built is when the build started, so an older read can never overwrite a newer one.
CREATE TABLE IF NOT EXISTS snapshot (id INTEGER PRIMARY KEY CHECK (id = 1), at INTEGER NOT NULL, built INTEGER NOT NULL DEFAULT 0, body TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS subscribers (
  email TEXT PRIMARY KEY, token TEXT NOT NULL UNIQUE, confirmed INTEGER NOT NULL DEFAULT 0,
  created INTEGER NOT NULL, last_mail INTEGER NOT NULL DEFAULT 0);
