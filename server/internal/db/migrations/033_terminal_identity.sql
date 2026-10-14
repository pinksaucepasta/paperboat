-- +goose Up
-- The primary key is the exact terminal identity used by signed runtime commands,
-- attachments, browser grants, and API URLs. The other fields never owned a
-- separate runtime lifecycle.
ALTER TABLE paperboat.user_machine_terminal_sessions
    DROP COLUMN terminal_id,
    DROP COLUMN thread_id;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
    RAISE EXCEPTION 'terminal identity consolidation is irreversible; roll back the release instead';
END $$;
-- +goose StatementEnd
