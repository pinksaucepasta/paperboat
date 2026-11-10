-- +goose Up
-- The per-file 50 MiB CHECK of file_transfers is not a product limit (Automatic is bounded by the server policy,
-- Unlimited has none). SQLite cannot drop a CHECK, so this is the standard table rebuild. user_version stays 1:
-- runtimes since 2026.10.26.1 also accept this table, so a failed update can still roll back to the previous build.
ALTER TABLE file_transfers RENAME TO file_transfers_old;
CREATE TABLE file_transfers(id TEXT PRIMARY KEY,batch_id TEXT NOT NULL,source_machine_id TEXT NOT NULL,destination_machine_id TEXT NOT NULL,initiating_user_id TEXT NOT NULL,session_id TEXT,delivery_client_id TEXT,basename TEXT NOT NULL,size INTEGER NOT NULL CHECK(size >= 0),sha256 TEXT NOT NULL CHECK(length(sha256)=64),committed_offset INTEGER NOT NULL CHECK(committed_offset BETWEEN 0 AND size),state TEXT NOT NULL CHECK(state IN ('created','uploading','published','pending','delivered','failed','canceled')),result_code TEXT,receipt_path TEXT,created_at INTEGER NOT NULL,expires_at INTEGER NOT NULL) STRICT;
INSERT INTO file_transfers SELECT * FROM file_transfers_old;
DROP TABLE file_transfers_old;
CREATE INDEX file_transfers_pending ON file_transfers(delivery_client_id,session_id,created_at) WHERE state='pending';
CREATE INDEX file_transfers_expiry ON file_transfers(expires_at);
