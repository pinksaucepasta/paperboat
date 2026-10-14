-- +goose Up
-- Revocation retains policy history; the machine/port hostname reservation
-- remains authoritative. Only a live policy occupies that hostname.
ALTER TABLE paperboat.lazy_access_policies
    DROP CONSTRAINT lazy_access_policies_hostname_key;

CREATE UNIQUE INDEX lazy_access_policies_live_hostname_unique
    ON paperboat.lazy_access_policies (hostname)
    WHERE deleted_at IS NULL;

-- +goose Down
-- Restoring the old constraint would require destroying revocation history.
-- +goose StatementBegin
DO $$ BEGIN
    RAISE EXCEPTION 'lazy policy regrant migration is irreversible; roll back the release instead';
END $$;
-- +goose StatementEnd
