-- +goose Up
-- A bug-report bundle is found under every support reference of the failures it carries, not only its own:
-- the client lists them explicitly when it asks to upload (diagnosticuploads.CreateRequest.FailureReferences),
-- so a reference printed for a client-only failure resolves once the user ran `pb bugreport --upload`.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.diagnostic_upload_intents ADD COLUMN failure_references text[] NOT NULL DEFAULT '{}';

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.diagnostic_upload_intents DROP COLUMN failure_references;
