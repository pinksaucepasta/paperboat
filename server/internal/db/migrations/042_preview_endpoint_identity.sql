-- +goose Up
ALTER TABLE paperboat.preview_leases DROP COLUMN endpoint_id;
