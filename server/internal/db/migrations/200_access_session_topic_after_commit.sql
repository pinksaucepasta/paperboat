-- +goose Up
-- A new access session no longer bumps its machine's machine_access topic from a trigger inside the session's
-- SERIALIZABLE transaction: every session of one machine updated the same topic row, so concurrent setups
-- conflicted (40001) and replayed until their retries ran out. The three code paths that insert a session publish
-- the topic right after their commit instead (usermachines.publishAccessSession, nativeprivateaccess). State changes
-- of a session keep their statement trigger.
DROP TRIGGER user_machine_access_sessions_publish_insert ON paperboat.user_machine_access_sessions;

-- +goose Down
-- +goose StatementBegin
CREATE TRIGGER user_machine_access_sessions_publish_insert AFTER INSERT ON paperboat.user_machine_access_sessions
    REFERENCING NEW TABLE AS new_sessions
    FOR EACH STATEMENT EXECUTE FUNCTION paperboat.publish_machine_access_sessions();
-- +goose StatementEnd
