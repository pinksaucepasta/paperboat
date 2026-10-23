-- +goose Up
-- One row naming the server instance that runs background workers (F21-04). Election itself is a
-- session advisory lock; this row is the observable record: who leads, since when, last heartbeat.
-- Prior art: Teleport lib/backend/lock.go stores the lock holder and expiry as a backend item.
CREATE TABLE paperboat.worker_leader (
    id boolean PRIMARY KEY DEFAULT true CHECK (id),
    instance text NOT NULL CHECK (length(instance) BETWEEN 1 AND 256),
    version text NOT NULL CHECK (length(version) BETWEEN 1 AND 128),
    since timestamptz NOT NULL,
    heartbeat_at timestamptz NOT NULL
);

-- +goose Down
DROP TABLE paperboat.worker_leader;
