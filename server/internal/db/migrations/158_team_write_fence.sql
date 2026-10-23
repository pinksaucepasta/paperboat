-- +goose Up
-- Team-touching writes serialize on one lock. It was pg_advisory_xact_lock(160235), taken as the first
-- statement of a SERIALIZABLE transaction. That statement also takes the transaction's snapshot before
-- waiting, so every waiter woke up with a stale snapshot and many failed with 40001 after retries
-- (40 parallel exec descriptors for one machine: 15 answered 503). LOCK TABLE is a utility command
-- that takes no snapshot, so the snapshot is taken after the lock is granted.
-- Prior art: PostgreSQL docs, Explicit Locking ("LOCK TABLE ... before any query in a SERIALIZABLE
-- transaction"); NetBird and Headscale serialize account writes with a row/table lock, not an
-- advisory lock taken after the snapshot.
CREATE TABLE paperboat.team_write_fence ();

-- +goose Down
DROP TABLE paperboat.team_write_fence;
